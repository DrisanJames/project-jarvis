package config

import (
	"os"
	"time"

	"github.com/joho/godotenv"
	"gopkg.in/yaml.v3"
)

// Config holds all configuration for the application
type Config struct {
	Server             ServerConfig            `yaml:"server"`
	SparkPost          SparkPostConfig         `yaml:"sparkpost"`
	Mailgun            MailgunConfig           `yaml:"mailgun"`
	SES                SESConfig               `yaml:"ses"`
	Ongage             OngageConfig            `yaml:"ongage"`
	Everflow           EverflowConfig          `yaml:"everflow"`
	OpenAI             OpenAIConfig            `yaml:"openai"`
	Azure              AzureConfig             `yaml:"azure"`
	Snowflake          SnowflakeConfig         `yaml:"snowflake"`
	Polling            PollingConfig           `yaml:"polling"`
	Storage            StorageConfig           `yaml:"storage"`
	Kanban             KanbanConfig            `yaml:"kanban"`
	Agent              AgentConfig             `yaml:"agent"`
	FallbackThresholds ThresholdConfig         `yaml:"fallback_thresholds"`
	ESPContracts       []ESPContract           `yaml:"esp_contracts"`
	RevenueModel       RevenueModelConfig      `yaml:"revenue_model"`
	IPPools            map[string][]IPPoolConfig `yaml:"ip_pools"`
	Auth               AuthConfig              `yaml:"auth"`
	Mailing            MailingConfig           `yaml:"mailing"`
	OVHCloud           OVHCloudConfig          `yaml:"ovhcloud"`
	DataNorm           DataNormConfig          `yaml:"datanorm"`
	Verification       VerificationConfig      `yaml:"verification"`
	Automation         AutomationConfig        `yaml:"automation"`
	Warmup             WarmupConfig            `yaml:"warmup"`
}

// DataNormConfig holds S3 data normalization settings (H14).
type DataNormConfig struct {
	Enabled         bool   `yaml:"enabled"`
	S3Bucket        string `yaml:"s3_bucket"`
	S3Region        string `yaml:"s3_region"`
	AWSProfile      string `yaml:"aws_profile"`
	IntervalMinutes int    `yaml:"interval_minutes"`
	DefaultListID   string `yaml:"default_list_id"`
}

// VerificationConfig holds email verification provider settings.
type VerificationConfig struct {
	Enabled       bool   `yaml:"enabled"`
	Provider      string `yaml:"provider"`
	APIKeyEnv     string `yaml:"api_key_env"`
	RatePerMinute int    `yaml:"rate_per_minute"`
}

// AutomationConfig holds automation flow engine settings.
type AutomationConfig struct {
	Enabled             bool `yaml:"enabled"`
	TickIntervalSeconds int  `yaml:"tick_interval_seconds"`
}

// WarmupConfig holds data quality thresholds for IP warmup tiers.
type WarmupConfig struct {
	DataQualitySeedThreshold     float64 `yaml:"data_quality_seed_threshold"`
	DataQualityValidateThreshold float64 `yaml:"data_quality_validate_threshold"`
	DataQualityExpandThreshold   float64 `yaml:"data_quality_expand_threshold"`
}

// OVHCloudConfig holds OVHCloud API credentials for dedicated server management.
type OVHCloudConfig struct {
	Endpoint          string `yaml:"endpoint"`
	ApplicationKey    string `yaml:"application_key"`
	ApplicationSecret string `yaml:"application_secret"`
	ConsumerKey       string `yaml:"consumer_key"`
}

// MailingConfig holds mailing platform configuration
type MailingConfig struct {
	Enabled            bool   `yaml:"enabled"`
	DatabaseURL        string `yaml:"database_url"`
	GoogleClientID     string `yaml:"google_client_id"`
	GoogleClientSecret string `yaml:"google_client_secret"`
	AllowedDomain      string `yaml:"allowed_domain"`
	SessionSecret      string `yaml:"session_secret"`
	CookieName         string `yaml:"cookie_name"`
	CookieMaxAge       int    `yaml:"cookie_max_age"`
}

// AuthConfig holds Google OAuth authentication configuration
type AuthConfig struct {
	Enabled            bool   `yaml:"enabled"`
	GoogleClientID     string `yaml:"google_client_id"`
	GoogleClientSecret string `yaml:"google_client_secret"`
	AllowedDomain      string `yaml:"allowed_domain"`
	SessionSecret      string `yaml:"session_secret"`
	CookieName         string `yaml:"cookie_name"`
	CookieMaxAge       int    `yaml:"cookie_max_age"`
}

// IPPoolConfig holds IP pool metadata
type IPPoolConfig struct {
	Name        string `yaml:"name"`
	Type        string `yaml:"type"`        // "dedicated" or "shared"
	Description string `yaml:"description"`
}

// RevenueModelConfig holds business unit P&L configuration
type RevenueModelConfig struct {
	Enabled          bool                   `yaml:"enabled"`
	FiscalYearStart  string                 `yaml:"fiscal_year_start"`  // Format: "2026-01"
	HistoricalCosts  map[string]float64     `yaml:"historical_costs"`   // Historical monthly costs by month key
	CostProjection   CostProjectionConfig   `yaml:"cost_projection"`
	VendorCosts      []VendorCost           `yaml:"vendor_costs"`
	RevenueShare     []RevenueShareItem     `yaml:"revenue_share"`
	ContractorCosts  []ContractorCost       `yaml:"contractor_costs"`
	EmployeeCosts    []EmployeeCost         `yaml:"employee_costs"`
}

// CostProjectionConfig holds cost projection parameters
type CostProjectionConfig struct {
	CurrentMax              float64 `yaml:"current_max"`                // Current maximum operational costs
	TargetReduction         float64 `yaml:"target_reduction"`           // Target cost reduction amount
	TargetCosts             float64 `yaml:"target_costs"`               // Target operational costs
	ReductionTimelineMonths int     `yaml:"reduction_timeline_months"`  // Months to achieve reduction
}

// VendorCost represents a monthly vendor/SaaS cost
type VendorCost struct {
	Name        string  `yaml:"name"`
	Category    string  `yaml:"category"`
	MonthlyCost float64 `yaml:"monthly_cost"`
}

// RevenueShareItem represents a revenue share obligation
type RevenueShareItem struct {
	Name          string  `yaml:"name"`
	MonthlyAmount float64 `yaml:"monthly_amount"`
}

// ContractorCost represents contractor payroll
type ContractorCost struct {
	Name        string  `yaml:"name"`
	MonthlyCost float64 `yaml:"monthly_cost"`
}

// EmployeeCost represents employee payroll (yearly salary)
type EmployeeCost struct {
	Name         string  `yaml:"name"`
	Role         string  `yaml:"role"`
	YearlySalary float64 `yaml:"yearly_salary"`
}

// ESPContract holds ESP pricing and contract details for cost calculations
type ESPContract struct {
	ESPName            string  `yaml:"esp_name"`              // "SparkPost", "Mailgun", "SES"
	MonthlyIncluded    int64   `yaml:"monthly_included"`      // Emails included in monthly fee (e.g., 200000000)
	MonthlyFee         float64 `yaml:"monthly_fee"`           // Monthly contract cost (e.g., 21120.00)
	OverageRatePer1000 float64 `yaml:"overage_rate_per_1000"` // Cost per 1000 extra emails (e.g., 0.13)
	Enabled            bool    `yaml:"enabled"`               // Whether this contract is active
}

// AzureConfig holds Azure Table Storage configuration for data injections
type AzureConfig struct {
	ConnectionString  string `yaml:"connection_string"`
	TableName         string `yaml:"table_name"`
	Enabled           bool   `yaml:"enabled"`
	GapThresholdHours int    `yaml:"gap_threshold_hours"`
}

// SnowflakeConfig holds Snowflake configuration for validation data
type SnowflakeConfig struct {
	ConnectionString string `yaml:"connection_string"`
	Account          string `yaml:"account"`
	User             string `yaml:"user"`
	Password         string `yaml:"password"`
	Database         string `yaml:"database"`
	Schema           string `yaml:"schema"`
	Warehouse        string `yaml:"warehouse"`
	Enabled          bool   `yaml:"enabled"`
}

// OpenAIConfig holds OpenAI API configuration for conversational agent
type OpenAIConfig struct {
	APIKey  string `yaml:"api_key"`
	Model   string `yaml:"model"`
	Enabled bool   `yaml:"enabled"`
}

// ServerConfig holds HTTP server configuration
type ServerConfig struct {
	Port int    `yaml:"port"`
	Host string `yaml:"host"`
}

// GetHost returns the server host, with ECS detection
func (c ServerConfig) GetHost() string {
	// On ECS/container, listen on all interfaces
	if os.Getenv("ECS_CONTAINER_METADATA_URI") != "" || os.Getenv("AWS_EXECUTION_ENV") != "" {
		return "0.0.0.0"
	}
	// Allow override via environment
	if host := os.Getenv("SERVER_HOST"); host != "" {
		return host
	}
	return c.Host
}

// SparkPostConfig holds SparkPost API configuration
type SparkPostConfig struct {
	APIKey         string `yaml:"api_key"`
	BaseURL        string `yaml:"base_url"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
}

// Timeout returns the configured timeout as a duration
func (c SparkPostConfig) Timeout() time.Duration {
	return time.Duration(c.TimeoutSeconds) * time.Second
}

// MailgunConfig holds Mailgun API configuration
type MailgunConfig struct {
	APIKey         string   `yaml:"api_key"`
	BaseURL        string   `yaml:"base_url"`
	TimeoutSeconds int      `yaml:"timeout_seconds"`
	Domains        []string `yaml:"domains"`
	Enabled        bool     `yaml:"enabled"`
}

// Timeout returns the configured timeout as a duration
func (c MailgunConfig) Timeout() time.Duration {
	return time.Duration(c.TimeoutSeconds) * time.Second
}

// SESConfig holds AWS SES API configuration
type SESConfig struct {
	Region         string   `yaml:"region"`
	AccessKey      string   `yaml:"access_key"`
	SecretKey      string   `yaml:"secret_key"`
	TimeoutSeconds int      `yaml:"timeout_seconds"`
	Enabled        bool     `yaml:"enabled"`
	ISPs           []string `yaml:"isps"` // ISPs to query for VDM metrics
}

// Timeout returns the configured timeout as a duration
func (c SESConfig) Timeout() time.Duration {
	return time.Duration(c.TimeoutSeconds) * time.Second
}

// DefaultISPs returns the default list of ISPs to query
func (c SESConfig) DefaultISPs() []string {
	if len(c.ISPs) > 0 {
		return c.ISPs
	}
	// Default major ISPs for VDM (AWS SES uses these exact names)
	return []string{
		"Att",
		"Yahoo",
		"Gmail",
		"Hotmail",
		"Aol",
		"Icloud",
		"Cox",
		"WP",
	}
}

// OngageConfig holds Ongage API configuration
type OngageConfig struct {
	BaseURL        string `yaml:"base_url"`
	Username       string `yaml:"username"`
	Password       string `yaml:"password"`
	AccountCode    string `yaml:"account_code"`
	ListID         string `yaml:"list_id"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
	Enabled        bool   `yaml:"enabled"`
	LookbackDays   int    `yaml:"lookback_days"`
}

// Timeout returns the configured timeout as a duration
func (c OngageConfig) Timeout() time.Duration {
	return time.Duration(c.TimeoutSeconds) * time.Second
}

// EverflowConfig holds Everflow API configuration
type EverflowConfig struct {
	APIKey       string   `yaml:"api_key"`
	BaseURL      string   `yaml:"base_url"`
	TimezoneID   int      `yaml:"timezone_id"`
	CurrencyID   string   `yaml:"currency_id"`
	Enabled      bool     `yaml:"enabled"`
	LookbackDays int      `yaml:"lookback_days"`
	AffiliateIDs []string `yaml:"affiliate_ids"`
}

// PollingConfig holds polling configuration
type PollingConfig struct {
	IntervalSeconds    int `yaml:"interval_seconds"`
	HistoricalDays     int `yaml:"historical_days"`
	AnalysisWindowDays int `yaml:"analysis_window_days"`
}

// Interval returns the polling interval as a duration
func (c PollingConfig) Interval() time.Duration {
	return time.Duration(c.IntervalSeconds) * time.Second
}

// StorageConfig holds storage configuration
type StorageConfig struct {
	Type          string `yaml:"type"`
	LocalPath     string `yaml:"local_path"`
	S3Bucket      string `yaml:"s3_bucket"`
	DynamoDBTable string `yaml:"dynamodb_table"`
	AWSRegion     string `yaml:"aws_region"`
	AWSProfile    string `yaml:"aws_profile"` // Empty string uses default credential chain (IAM role on ECS)
}

// GetAWSProfile returns the AWS profile, with environment variable override
func (c StorageConfig) GetAWSProfile() string {
	if envProfile := os.Getenv("AWS_PROFILE_OVERRIDE"); envProfile != "" {
		if envProfile == "none" || envProfile == "iam" {
			return "" // Use default credential chain (IAM role)
		}
		return envProfile
	}
	// On ECS/Lambda, don't use a profile - use IAM role
	if os.Getenv("ECS_CONTAINER_METADATA_URI") != "" || os.Getenv("AWS_EXECUTION_ENV") != "" {
		return "" // Running on ECS or Lambda, use IAM role
	}
	return c.AWSProfile
}

// KanbanConfig holds Kanban task management configuration
type KanbanConfig struct {
	Enabled           bool   `yaml:"enabled"`
	DynamoDBTable     string `yaml:"dynamodb_table"` // Uses storage table if empty
	MaxActiveTasks    int    `yaml:"max_active_tasks"`
	MaxNewTasksPerRun int    `yaml:"max_new_tasks_per_run"`
	AIRunIntervalMins int    `yaml:"ai_run_interval_mins"`
}

// AgentConfig holds learning agent configuration
type AgentConfig struct {
	BaselineRecalcHours    int     `yaml:"baseline_recalc_hours"`
	CorrelationRecalcHours int     `yaml:"correlation_recalc_hours"`
	AnomalySigma           float64 `yaml:"anomaly_sigma"`
	MinDataPoints          int     `yaml:"min_data_points"`
}

// ThresholdConfig holds fallback threshold configuration
type ThresholdConfig struct {
	ComplaintRateWarning  float64 `yaml:"complaint_rate_warning"`
	ComplaintRateCritical float64 `yaml:"complaint_rate_critical"`
	BounceRateWarning     float64 `yaml:"bounce_rate_warning"`
	BounceRateCritical    float64 `yaml:"bounce_rate_critical"`
	BlockRateWarning      float64 `yaml:"block_rate_warning"`
	BlockRateCritical     float64 `yaml:"block_rate_critical"`
}

// Load reads and parses the configuration file
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	// Set defaults
	if cfg.Server.Port == 0 {
		cfg.Server.Port = 8080
	}
	if cfg.Server.Host == "" {
		cfg.Server.Host = "localhost"
	}
	if cfg.SparkPost.TimeoutSeconds == 0 {
		cfg.SparkPost.TimeoutSeconds = 30
	}
	if cfg.Mailgun.TimeoutSeconds == 0 {
		cfg.Mailgun.TimeoutSeconds = 30
	}
	if cfg.Mailgun.BaseURL == "" {
		cfg.Mailgun.BaseURL = "https://api.mailgun.net"
	}
	if cfg.SES.TimeoutSeconds == 0 {
		cfg.SES.TimeoutSeconds = 30
	}
	if cfg.SES.Region == "" {
		cfg.SES.Region = "us-west-2"
	}
	if cfg.Ongage.TimeoutSeconds == 0 {
		cfg.Ongage.TimeoutSeconds = 60
	}
	if cfg.Ongage.LookbackDays == 0 {
		cfg.Ongage.LookbackDays = 30
	}
	if cfg.Everflow.BaseURL == "" {
		cfg.Everflow.BaseURL = "https://api.eflow.team"
	}
	if cfg.Everflow.TimezoneID == 0 {
		cfg.Everflow.TimezoneID = 90
	}
	if cfg.Everflow.CurrencyID == "" {
		cfg.Everflow.CurrencyID = "USD"
	}
	if cfg.Everflow.LookbackDays == 0 {
		cfg.Everflow.LookbackDays = 30
	}
	if cfg.OpenAI.Model == "" {
		cfg.OpenAI.Model = "gpt-4o"
	}
	// Azure Table Storage defaults
	if cfg.Azure.TableName == "" {
		cfg.Azure.TableName = "ignitemediagroupcrm"
	}
	if cfg.Azure.GapThresholdHours == 0 {
		cfg.Azure.GapThresholdHours = 24
	}
	// Snowflake defaults
	if cfg.Snowflake.Database == "" {
		cfg.Snowflake.Database = "IGNITE_DATA_LAKE"
	}
	if cfg.Snowflake.Schema == "" {
		cfg.Snowflake.Schema = "REFINEDEMAILS"
	}
	if cfg.Polling.IntervalSeconds == 0 {
		cfg.Polling.IntervalSeconds = 60
	}
	if cfg.Polling.HistoricalDays == 0 {
		cfg.Polling.HistoricalDays = 30
	}
	if cfg.Polling.AnalysisWindowDays == 0 {
		cfg.Polling.AnalysisWindowDays = 7
	}
	if cfg.Agent.AnomalySigma == 0 {
		cfg.Agent.AnomalySigma = 2.0
	}
	if cfg.Agent.MinDataPoints == 0 {
		cfg.Agent.MinDataPoints = 100
	}

	return &cfg, nil
}

// LoadFromEnv loads configuration with environment variable overrides.
// It automatically loads a .env file (if present) before reading env vars,
// so secrets can live in .env locally and in real env vars on ECS.
func LoadFromEnv(path string) (*Config, error) {
	// Load .env file if it exists (no error if missing)
	_ = godotenv.Load()

	cfg, err := Load(path)
	if err != nil {
		return nil, err
	}

	// Override with environment variables if present
	if apiKey := os.Getenv("SPARKPOST_API_KEY"); apiKey != "" {
		cfg.SparkPost.APIKey = apiKey
	}
	if baseURL := os.Getenv("SPARKPOST_BASE_URL"); baseURL != "" {
		cfg.SparkPost.BaseURL = baseURL
	}
	if apiKey := os.Getenv("MAILGUN_API_KEY"); apiKey != "" {
		cfg.Mailgun.APIKey = apiKey
	}
	if baseURL := os.Getenv("MAILGUN_BASE_URL"); baseURL != "" {
		cfg.Mailgun.BaseURL = baseURL
	}
	if accessKey := os.Getenv("AWS_SES_ACCESS_KEY"); accessKey != "" {
		cfg.SES.AccessKey = accessKey
	}
	if secretKey := os.Getenv("AWS_SES_SECRET_KEY"); secretKey != "" {
		cfg.SES.SecretKey = secretKey
	}
	if region := os.Getenv("AWS_SES_REGION"); region != "" {
		cfg.SES.Region = region
	}
	if baseURL := os.Getenv("ONGAGE_BASE_URL"); baseURL != "" {
		cfg.Ongage.BaseURL = baseURL
	}
	if username := os.Getenv("ONGAGE_USERNAME"); username != "" {
		cfg.Ongage.Username = username
	}
	if password := os.Getenv("ONGAGE_PASSWORD"); password != "" {
		cfg.Ongage.Password = password
	}
	if accountCode := os.Getenv("ONGAGE_ACCOUNT_CODE"); accountCode != "" {
		cfg.Ongage.AccountCode = accountCode
	}
	if apiKey := os.Getenv("EVERFLOW_API_KEY"); apiKey != "" {
		cfg.Everflow.APIKey = apiKey
	}
	if baseURL := os.Getenv("EVERFLOW_BASE_URL"); baseURL != "" {
		cfg.Everflow.BaseURL = baseURL
	}

	// Database override (critical for ECS deployment where config.yaml has local defaults)
	if dbURL := os.Getenv("DATABASE_URL"); dbURL != "" {
		cfg.Mailing.DatabaseURL = dbURL
		if !cfg.Mailing.Enabled {
			cfg.Mailing.Enabled = true
		}
	}

	// Auth overrides
	if v := os.Getenv("GOOGLE_CLIENT_ID"); v != "" {
		cfg.Auth.GoogleClientID = v
	}
	if v := os.Getenv("GOOGLE_CLIENT_SECRET"); v != "" {
		cfg.Auth.GoogleClientSecret = v
	}
	if v := os.Getenv("SESSION_SECRET"); v != "" {
		cfg.Auth.SessionSecret = v
	}
	if v := os.Getenv("AUTH_ALLOWED_DOMAIN"); v != "" {
		cfg.Auth.AllowedDomain = v
	}
	// DataNorm overrides
	if v := os.Getenv("DATANORM_S3_BUCKET"); v != "" {
		cfg.DataNorm.S3Bucket = v
	}
	if v := os.Getenv("DATANORM_S3_REGION"); v != "" {
		cfg.DataNorm.S3Region = v
	}

	return cfg, nil
}
