package ongage

import (
	"encoding/json"
	"fmt"
	"time"
)

// FlexString is a string type that can unmarshal from both string and number JSON values
type FlexString string

// UnmarshalJSON implements json.Unmarshaler for FlexString
func (f *FlexString) UnmarshalJSON(data []byte) error {
	// Try string first
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*f = FlexString(s)
		return nil
	}

	// Try number (int or float)
	var n json.Number
	if err := json.Unmarshal(data, &n); err == nil {
		*f = FlexString(n.String())
		return nil
	}

	return fmt.Errorf("FlexString: cannot unmarshal %s", string(data))
}

// String returns the string value
func (f FlexString) String() string {
	return string(f)
}

// Config holds Ongage API configuration
type Config struct {
	BaseURL     string `yaml:"base_url"`
	Username    string `yaml:"username"`
	Password    string `yaml:"password"`
	AccountCode string `yaml:"account_code"`
	ListID      string `yaml:"list_id"` // Default list ID for API calls
}

// APIResponse is the base response structure from Ongage API
type APIResponse struct {
	Metadata ResponseMetadata `json:"metadata"`
	Payload  interface{}      `json:"payload"`
}

// ResponseMetadata contains error info from API responses
type ResponseMetadata struct {
	Error bool        `json:"error"`
	Total interface{} `json:"total,omitempty"` // Can be string or number
}

// ========== Campaign/Mailing Types ==========

// Campaign represents a mailing campaign from Ongage
type Campaign struct {
	ID                   string             `json:"id"`
	Name                 string             `json:"name"`
	Description          string             `json:"description,omitempty"`
	Type                 string             `json:"type"` // campaign, split
	SplitType            string             `json:"split_type,omitempty"`
	ListID               string             `json:"list_id"`
	IsTest               string             `json:"is_test"` // "0" or "1"
	ScheduleDate         string             `json:"schedule_date"`
	Status               string             `json:"status"`      // e.g., "60004"
	StatusDesc           string             `json:"status_desc"` // e.g., "Completed"
	StatusDate           string             `json:"status_date"`
	Progress             string             `json:"progress,omitempty"`
	Created              string             `json:"created"`
	Modified             string             `json:"modified"`
	Deleted              string             `json:"deleted"`
	ScheduledBy          string             `json:"scheduled_by,omitempty"`
	SendingStartDate     string             `json:"sending_start_date,omitempty"`
	SendingEndDate       string             `json:"sending_end_date,omitempty"`
	Targeted             string             `json:"targeted,omitempty"`
	ESPs                 string             `json:"esps,omitempty"`
	Comment              string             `json:"comment,omitempty"`
	EmailID              string             `json:"email_id,omitempty"`
	EmailName            string             `json:"email_name,omitempty"`
	MessageType          string             `json:"message_type,omitempty"`
	Segments             []CampaignSegment  `json:"segments,omitempty"`
	Distribution         []ESPDistribution  `json:"distribution,omitempty"`
	EmailMessages        []EmailMessage     `json:"email_message,omitempty"`
	ESPConnectionsQuota  []interface{}      `json:"esp_connections_quota,omitempty"`
}

// CampaignSegment represents a segment associated with a campaign
type CampaignSegment struct {
	SegmentID string `json:"segment_id"`
	Type      string `json:"type"`
	Name      string `json:"name"`
	LastCount string `json:"last_count,omitempty"`
	Exclude   string `json:"exclude,omitempty"` // "0" or "1"
}

// ESPDistribution represents ESP routing configuration
// Note: esp_id can come as either string or number from Ongage API
type ESPDistribution struct {
	ESPID           FlexString `json:"esp_id"`
	ESPConnectionID FlexString `json:"esp_connection_id"`
	ISPID           FlexString `json:"isp_id,omitempty"`
	Domain          string     `json:"domain,omitempty"`
	Percent         FlexString `json:"percent"`
	Name            string     `json:"name,omitempty"`
	SegmentID       FlexString `json:"segment_id,omitempty"`
}

// EmailMessage represents an email message/creative
type EmailMessage struct {
	Type           string `json:"type"` // email_message
	EmailMessageID string `json:"email_message_id"`
	Name           string `json:"name"`
	Description    string `json:"description,omitempty"`
	Subject        string `json:"subject"`
	Preview        string `json:"preview,omitempty"`
}

// CampaignListResponse is the response for GET /api/mailings
type CampaignListResponse struct {
	Metadata ResponseMetadata `json:"metadata"`
	Payload  []Campaign       `json:"payload"`
}

// CampaignDetailResponse is the response for GET /api/mailings/{id}
type CampaignDetailResponse struct {
	Metadata ResponseMetadata `json:"metadata"`
	Payload  Campaign         `json:"payload"`
}

// CampaignStatus constants
const (
	StatusNew                 = "60001"
	StatusScheduled           = "60002"
	StatusInProgress          = "60003"
	StatusCompleted           = "60004"
	StatusError               = "60005"
	StatusCancelled           = "60006"
	StatusDeleted             = "60007"
	StatusCompletedWithErrors = "60008"
	StatusOnHold              = "60009"
	StatusStopped             = "60010"
)

// StatusDescriptions maps status codes to human-readable descriptions
var StatusDescriptions = map[string]string{
	StatusNew:                 "New",
	StatusScheduled:           "Scheduled",
	StatusInProgress:          "In Progress",
	StatusCompleted:           "Completed",
	StatusError:               "Error",
	StatusCancelled:           "Cancelled",
	StatusDeleted:             "Deleted",
	StatusCompletedWithErrors: "Completed With Errors",
	StatusOnHold:              "On Hold",
	StatusStopped:             "Stopped",
}

// ========== Report Types ==========

// ReportQuery represents a query to the reports API
type ReportQuery struct {
	Select         []interface{}   `json:"select"`
	From           string          `json:"from"` // "mailing" or "list"
	Filter         [][]interface{} `json:"filter,omitempty"`
	Group          []interface{}   `json:"group,omitempty"`
	Order          []interface{}   `json:"order,omitempty"`
	ListIDs        interface{}     `json:"list_ids,omitempty"` // "all" or []int
	CalculateRates bool            `json:"calculate_rates,omitempty"`
	TimeZone       string          `json:"time_zone,omitempty"`
	Offset         int             `json:"offset,omitempty"`
	Limit          int             `json:"limit,omitempty"`
}

// ReportResponse is the response from POST /api/reports/query
type ReportResponse struct {
	Metadata ResponseMetadata `json:"metadata"`
	Payload  []ReportRow      `json:"payload"`
}

// ReportRow represents a single row in a report response
// Fields are dynamic based on the query
type ReportRow map[string]interface{}

// CampaignStats represents aggregated campaign statistics
type CampaignStats struct {
	MailingID         string  `json:"mailing_id"`
	MailingName       string  `json:"mailing_name"`
	EmailSubject      string  `json:"email_message_subject,omitempty"`
	ScheduleDate      string  `json:"schedule_date,omitempty"`
	StatsDate         string  `json:"stats_date,omitempty"`
	ESPName           string  `json:"esp_name,omitempty"`
	ESPConnectionID   string  `json:"esp_connection_id,omitempty"`
	ESPConnectionTitle string `json:"esp_connection_title,omitempty"`
	SegmentName       string  `json:"segment_name,omitempty"`
	ISPName           string  `json:"isp_name,omitempty"`
	Targeted          int64   `json:"targeted"`
	Sent              int64   `json:"sent"`
	Success           int64   `json:"success"`
	Failed            int64   `json:"failed"`
	Opens             int64   `json:"opens"`
	UniqueOpens       int64   `json:"unique_opens"`
	Clicks            int64   `json:"clicks"`
	UniqueClicks      int64   `json:"unique_clicks"`
	Unsubscribes      int64   `json:"unsubscribes"`
	Complaints        int64   `json:"complaints"`
	HardBounces       int64   `json:"hard_bounces"`
	SoftBounces       int64   `json:"soft_bounces"`
}

// ========== ESP Connection Types ==========

// ESPConnection represents an ESP vendor connection
type ESPConnection struct {
	ID          string     `json:"id"`
	ESPID       FlexString `json:"esp_id"`
	Name        string     `json:"name"`
	Title       string     `json:"title,omitempty"`
	Active      string     `json:"active,omitempty"`
	AccountID   string     `json:"account_id,omitempty"`
	IsDefault   string     `json:"is_default,omitempty"`
}

// ESPConnectionResponse is the response for GET /api/esp_connections/options
type ESPConnectionResponse struct {
	Metadata ResponseMetadata `json:"metadata"`
	Payload  []ESPConnection  `json:"payload"`
}

// ========== List Metadata Types ==========

// ListInfo represents basic metadata about an Ongage list
type ListInfo struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// ListInfoResponse is the response for GET /api/lists
type ListInfoResponse struct {
	Metadata ResponseMetadata `json:"metadata"`
	Payload  []ListInfo       `json:"payload"`
}

// ESP IDs for known vendors
const (
	ESPIDAmazonSES         = "4"
	ESPIDMailgun           = "58"
	ESPIDSparkPost         = "86"
	ESPIDSparkPostEnterprise = "93"
	ESPIDSparkPostMomentum = "110"
)

// ESPNames maps ESP IDs to vendor names
var ESPNames = map[string]string{
	ESPIDAmazonSES:         "Amazon SES",
	ESPIDMailgun:           "Mailgun",
	ESPIDSparkPost:         "SparkPost",
	ESPIDSparkPostEnterprise: "SparkPost Enterprise",
	ESPIDSparkPostMomentum: "SparkPost Momentum",
}

// ========== Import Types ==========

// ImportJob represents an import job status
type ImportJob struct {
	ID             string `json:"id"`
	ListID         string `json:"list_id"`
	Status         string `json:"status"`
	StatusDesc     string `json:"status_desc,omitempty"`
	TotalRecords   int64  `json:"total_records"`
	ImportedCount  int64  `json:"imported_count"`
	FailedCount    int64  `json:"failed_count"`
	SkippedCount   int64  `json:"skipped_count"`
	Created        string `json:"created"`
	Completed      string `json:"completed,omitempty"`
}

// ListStats represents list-level statistics
type ListStats struct {
	ListID       string `json:"list_id"`
	Active       int64  `json:"active"`
	NotActive    int64  `json:"not_active"`
	Complaints   int64  `json:"complaints"`
	Unsubscribes int64  `json:"unsubscribes"`
	Bounces      int64  `json:"bounces"`
	Opened       int64  `json:"opened"`
	Clicked      int64  `json:"clicked"`
	NoActivity   int64  `json:"no_activity"`
	RecordDate   string `json:"record_date,omitempty"`
}

// ========== Processed/Computed Types ==========

// ProcessedCampaign represents a campaign with computed metrics
type ProcessedCampaign struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	Subject          string    `json:"subject"`
	Status           string    `json:"status"`
	StatusDesc       string    `json:"status_desc"`
	ScheduleTime     time.Time `json:"schedule_time"`
	SendStartTime    time.Time `json:"send_start_time,omitempty"`
	SendEndTime      time.Time `json:"send_end_time,omitempty"`
	ESP              string    `json:"esp"`
	ESPConnectionID  string    `json:"esp_connection_id"`
	Segments         []string  `json:"segments"`
	Targeted         int64     `json:"targeted"`
	Sent             int64     `json:"sent"`
	Delivered        int64     `json:"delivered"`
	DeliveryRate     float64   `json:"delivery_rate"`
	Opens            int64     `json:"opens"`
	UniqueOpens      int64     `json:"unique_opens"`
	OpenRate         float64   `json:"open_rate"`
	Clicks           int64     `json:"clicks"`
	UniqueClicks     int64     `json:"unique_clicks"`
	ClickRate        float64   `json:"click_rate"`
	CTR              float64   `json:"ctr"` // Click-to-open rate
	Unsubscribes     int64     `json:"unsubscribes"`
	UnsubscribeRate  float64   `json:"unsubscribe_rate"`
	Complaints       int64     `json:"complaints"`
	ComplaintRate    float64   `json:"complaint_rate"`
	Bounces          int64     `json:"bounces"`
	BounceRate       float64   `json:"bounce_rate"`
	Failed           int64     `json:"failed"`           // Non-bounce failures (connection errors, etc.)
	HardBounces      int64     `json:"hard_bounces"`     // Permanent delivery failures
	SoftBounces      int64     `json:"soft_bounces"`     // Temporary delivery failures
	IsTest           bool      `json:"is_test"`
}

// SubjectLineAnalysis represents analysis of a subject line's performance
type SubjectLineAnalysis struct {
	Subject       string   `json:"subject"`
	CampaignCount int      `json:"campaign_count"`
	TotalSent     int64    `json:"total_sent"`
	AvgOpenRate   float64  `json:"avg_open_rate"`
	AvgClickRate  float64  `json:"avg_click_rate"`
	AvgCTR        float64  `json:"avg_ctr"`
	Length        int      `json:"length"`
	HasEmoji      bool     `json:"has_emoji"`
	HasNumber     bool     `json:"has_number"`
	HasQuestion   bool     `json:"has_question"`
	HasUrgency    bool     `json:"has_urgency"`
	ESPs          []string `json:"esps"`
	Performance   string   `json:"performance"` // "high", "medium", "low"
}

// ScheduleAnalysis represents send time performance analysis
type ScheduleAnalysis struct {
	Hour           int     `json:"hour"`
	DayOfWeek      int     `json:"day_of_week"`
	DayName        string  `json:"day_name"`
	CampaignCount  int     `json:"campaign_count"`
	TotalSent      int64   `json:"total_sent"`
	AvgOpenRate    float64 `json:"avg_open_rate"`
	AvgClickRate   float64 `json:"avg_click_rate"`
	AvgDeliveryRate float64 `json:"avg_delivery_rate"`
	Performance    string  `json:"performance"` // "optimal", "good", "average", "poor"
}

// ESPPerformance represents ESP-specific performance metrics
type ESPPerformance struct {
	ESPID           string  `json:"esp_id"`
	ESPName         string  `json:"esp_name"`
	ConnectionID    string  `json:"connection_id"`
	ConnectionTitle string  `json:"connection_title"`
	CampaignCount   int     `json:"campaign_count"`
	TotalSent       int64   `json:"total_sent"`
	TotalDelivered  int64   `json:"total_delivered"`
	DeliveryRate    float64 `json:"delivery_rate"`
	OpenRate        float64 `json:"open_rate"`
	ClickRate       float64 `json:"click_rate"`
	BounceRate      float64 `json:"bounce_rate"`
	ComplaintRate   float64 `json:"complaint_rate"`
}

// AudienceAnalysis represents segment/audience performance
type AudienceAnalysis struct {
	SegmentID     string  `json:"segment_id"`
	SegmentName   string  `json:"segment_name"`
	CampaignCount int     `json:"campaign_count"`
	TotalTargeted int64   `json:"total_targeted"`
	TotalSent     int64   `json:"total_sent"`
	AvgOpenRate   float64 `json:"avg_open_rate"`
	AvgClickRate  float64 `json:"avg_click_rate"`
	AvgBounceRate float64 `json:"avg_bounce_rate"`
	Engagement    string  `json:"engagement"` // "high", "medium", "low"
}

// PipelineMetrics represents end-to-end pipeline metrics
type PipelineMetrics struct {
	Date            string  `json:"date"`
	ImportsCount    int     `json:"imports_count"`
	RecordsImported int64   `json:"records_imported"`
	CampaignsSent   int     `json:"campaigns_sent"`
	TotalTargeted   int64   `json:"total_targeted"`
	TotalSent       int64   `json:"total_sent"`
	TotalDelivered  int64   `json:"total_delivered"`
	DeliveryRate    float64 `json:"delivery_rate"`
	TotalOpens      int64   `json:"total_opens"`
	OpenRate        float64 `json:"open_rate"`
	TotalClicks     int64   `json:"total_clicks"`
	ClickRate       float64 `json:"click_rate"`
}

// CollectorMetrics represents the latest collected Ongage metrics
type CollectorMetrics struct {
	Campaigns         []ProcessedCampaign   `json:"campaigns"`
	ESPConnections    []ESPConnection       `json:"esp_connections"`
	SubjectAnalysis   []SubjectLineAnalysis `json:"subject_analysis"`
	ScheduleAnalysis  []ScheduleAnalysis    `json:"schedule_analysis"`
	ESPPerformance    []ESPPerformance      `json:"esp_performance"`
	AudienceAnalysis  []AudienceAnalysis    `json:"audience_analysis"`
	PipelineMetrics   []PipelineMetrics     `json:"pipeline_metrics"`
	LastFetch         time.Time             `json:"last_fetch"`
	TotalCampaigns    int                   `json:"total_campaigns"`
	ActiveCampaigns   int                   `json:"active_campaigns"`
}

// ========== Import Types ==========

// Import represents an import job from Ongage
type Import struct {
	ID                      string `json:"id"`
	Action                  string `json:"action"`                     // add, remove, update
	Name                    string `json:"name"`                       // filename
	File                    string `json:"file"`
	FileURL                 string `json:"file_url"`
	ImportedBy              string `json:"imported_by"`
	IsOverride              string `json:"is_override"`
	SendWelcomeMessage      string `json:"send_welcome_message"`
	SendEmailNotification   string `json:"send_email_notification"`
	CSVDelimiter            string `json:"csv_delimiter"`
	Encoding                string `json:"encoding"`
	IgnoreEmpty             string `json:"ignore_empty"`
	OverwriteOnlyNulls      string `json:"overwrite_only_nulls"`
	Fields                  string `json:"fields"`                     // JSON string
	Progress                string `json:"progress"`                   // percentage
	FileSizeBytes           string `json:"file_size_bytes"`
	Total                   string `json:"total"`                      // total records
	Success                 string `json:"success"`                    // successfully imported
	Failed                  string `json:"failed"`
	Duplicate               string `json:"duplicate"`
	Existing                string `json:"existing"`
	NotExisting             string `json:"not_existing"`
	Incomplete              string `json:"incomplete"`
	Invalid                 string `json:"invalid"`
	Status                  string `json:"status"`                     // status code
	StatusInfo              string `json:"status_info"`
	Remaining               string `json:"remaining"`
	ImportProcessStartTime  string `json:"import_process_start_time"`  // unix timestamp
	ImportProcessEndTime    string `json:"import_process_end_time"`    // unix timestamp
	AccountID               string `json:"account_id"`
	ListID                  string `json:"list_id"`
	Created                 string `json:"created"`                    // unix timestamp
	Modified                string `json:"modified"`                   // unix timestamp
	Deleted                 string `json:"deleted"`
	CanBeAborted            string `json:"can_be_aborted"`
	Type                    string `json:"type"`                       // sending, etc.
	StatusDesc              string `json:"status_desc"`                // "Processing (46%)"
}

// ImportResponse is the response from the imports API
type ImportResponse struct {
	Metadata ResponseMetadata `json:"metadata"`
	Payload  []Import         `json:"payload"`
}

// SingleImportResponse is the response when fetching a single import
type SingleImportResponse struct {
	Metadata ResponseMetadata `json:"metadata"`
	Payload  Import           `json:"payload"`
}

// ImportMetrics represents aggregated import metrics
type ImportMetrics struct {
	TotalImports    int   `json:"total_imports"`
	TodayImports    int   `json:"today_imports"`
	TotalRecords    int64 `json:"total_records"`
	SuccessRecords  int64 `json:"success_records"`
	FailedRecords   int64 `json:"failed_records"`
	DuplicateRecords int64 `json:"duplicate_records"`
	ExistingRecords int64 `json:"existing_records"`
	InProgress      int   `json:"in_progress"`
	Completed       int   `json:"completed"`
}

// DailyImportMetrics represents daily import metrics
type DailyImportMetrics struct {
	Date            string `json:"date"`
	TotalImports    int    `json:"total_imports"`
	TotalRecords    int64  `json:"total_records"`
	SuccessRecords  int64  `json:"success_records"`
	FailedRecords   int64  `json:"failed_records"`
	DuplicateRecords int64 `json:"duplicate_records"`
}

// ImportSummary provides an overview of import activity
type ImportSummary struct {
	Timestamp     time.Time            `json:"timestamp"`
	Metrics       ImportMetrics        `json:"metrics"`
	DailyMetrics  []DailyImportMetrics `json:"daily_metrics"`
	RecentImports []Import             `json:"recent_imports"`
}

// ========== Contact Activity Types ==========

// ContactActivityRequest represents the request body for POST /api/contact_activity
type ContactActivityRequest struct {
	Title          string                  `json:"title"`
	SelectedFields []string                `json:"selected_fields"`
	Filters        ContactActivityFilters  `json:"filters"`
	CombinedAsAnd  bool                    `json:"combined_as_and,omitempty"`
	IncludeBehavior bool                   `json:"include_behavior,omitempty"`
}

// ContactActivityFilters contains the filter configuration for a contact activity report
type ContactActivityFilters struct {
	Criteria []ContactActivityCriterion `json:"criteria"`
	UserType string                     `json:"user_type"` // "all", "active", etc.
	FromDate int64                      `json:"from_date"` // Unix timestamp
	ToDate   int64                      `json:"to_date"`   // Unix timestamp
}

// ContactActivityCriterion is a single filter criterion
type ContactActivityCriterion struct {
	FieldName     string      `json:"field_name"`
	Type          string      `json:"type"`           // "string", "numeric", "email", "date", "id", "segment", "behavioral"
	Operator      string      `json:"operator"`       // "=", "!=", "notempty", "empty", "LIKE", etc.
	Operand       interface{} `json:"operand"`        // array of values, or object for behavioral
	CaseSensitive int         `json:"case_sensitive"` // 0 or 1
	Condition     string      `json:"condition"`      // "and", "or"
}

// ContactActivityCreateResponse is the response from POST /api/contact_activity
type ContactActivityCreateResponse struct {
	Metadata ResponseMetadata       `json:"metadata"`
	Payload  ContactActivityPayload `json:"payload"`
}

// ContactActivityPayload contains the created report info
type ContactActivityPayload struct {
	ID     json.Number `json:"id"`
	Status int         `json:"status"` // 1 = Pending, 2 = Completed
}

// ContactActivityStatusResponse is the response from GET /api/contact_activity/{id}
type ContactActivityStatusResponse struct {
	Metadata ResponseMetadata       `json:"metadata"`
	Payload  ContactActivityPayload `json:"payload"`
}
