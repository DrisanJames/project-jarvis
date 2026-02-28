package engine

import (
	"encoding/json"
	"fmt"
	"time"
)

// ISP identifies one of the 8 supported ISP groups.
type ISP string

const (
	ISPGmail     ISP = "gmail"
	ISPYahoo     ISP = "yahoo"
	ISPMicrosoft ISP = "microsoft"
	ISPApple     ISP = "apple"
	ISPComcast   ISP = "comcast"
	ISPAtt       ISP = "att"
	ISPCox       ISP = "cox"
	ISPCharter   ISP = "charter"
)

// AllISPs returns all 8 supported ISP groups.
func AllISPs() []ISP {
	return []ISP{ISPGmail, ISPYahoo, ISPMicrosoft, ISPApple, ISPComcast, ISPAtt, ISPCox, ISPCharter}
}

// AgentType identifies one of the 6 agent types.
type AgentType string

const (
	AgentReputation  AgentType = "reputation"
	AgentThrottle    AgentType = "throttle"
	AgentPool        AgentType = "pool"
	AgentWarmup      AgentType = "warmup"
	AgentEmergency   AgentType = "emergency"
	AgentSuppression AgentType = "suppression"
)

// AllAgentTypes returns all 6 agent types.
func AllAgentTypes() []AgentType {
	return []AgentType{AgentReputation, AgentThrottle, AgentPool, AgentWarmup, AgentEmergency, AgentSuppression}
}

// AgentID uniquely identifies an agent instance (ISP + Type).
type AgentID struct {
	ISP       ISP       `json:"isp"`
	AgentType AgentType `json:"agent_type"`
}

func (id AgentID) String() string {
	return string(id.ISP) + "/" + string(id.AgentType)
}

// AgentStatus represents the current state of an agent.
type AgentStatus string

const (
	StatusActive   AgentStatus = "active"
	StatusPaused   AgentStatus = "paused"
	StatusFiring   AgentStatus = "firing"
	StatusError    AgentStatus = "error"
	StatusCooldown AgentStatus = "cooldown"
)

// ISPConfig holds per-ISP configuration thresholds and settings.
type ISPConfig struct {
	ID               string          `json:"id" db:"id"`
	OrganizationID   string          `json:"organization_id" db:"organization_id"`
	ISP              ISP             `json:"isp" db:"isp"`
	DisplayName      string          `json:"display_name" db:"display_name"`
	DomainPatterns   []string        `json:"domain_patterns"`
	MXPatterns       []string        `json:"mx_patterns"`
	BounceWarnPct    float64         `json:"bounce_warn_pct" db:"bounce_warn_pct"`
	BounceActionPct  float64         `json:"bounce_action_pct" db:"bounce_action_pct"`
	ComplaintWarnPct float64         `json:"complaint_warn_pct" db:"complaint_warn_pct"`
	ComplaintActionPct float64       `json:"complaint_action_pct" db:"complaint_action_pct"`
	MaxConnections   int             `json:"max_connections" db:"max_connections"`
	MaxMsgRate       int             `json:"max_msg_rate" db:"max_msg_rate"`
	DeferralCodes    []string        `json:"deferral_codes"`
	KnownBehaviors   json.RawMessage `json:"known_behaviors" db:"known_behaviors"`
	PoolName         string          `json:"pool_name" db:"pool_name"`
	WarmupSchedule   json.RawMessage `json:"warmup_schedule" db:"warmup_schedule"`
	Enabled          bool            `json:"enabled" db:"enabled"`
	CreatedAt        time.Time       `json:"created_at" db:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at" db:"updated_at"`
}

// Signal represents a computed metric value over a time window.
type Signal struct {
	ID             string    `json:"id"`
	OrganizationID string    `json:"organization_id"`
	ISP            ISP       `json:"isp"`
	MetricName     string    `json:"metric_name"`
	DimensionType  string    `json:"dimension_type"`
	DimensionValue string    `json:"dimension_value"`
	Value          float64   `json:"value"`
	WindowSeconds  int       `json:"window_seconds"`
	SampleCount    int       `json:"sample_count"`
	RecordedAt     time.Time `json:"recorded_at"`
}

// Rule defines a governance rule scoped to ISP + AgentType.
type Rule struct {
	ID              string          `json:"id" db:"id"`
	OrganizationID  string          `json:"organization_id" db:"organization_id"`
	ISP             ISP             `json:"isp" db:"isp"`
	AgentType       AgentType       `json:"agent_type" db:"agent_type"`
	Name            string          `json:"name" db:"name"`
	Description     string          `json:"description" db:"description"`
	Metric          string          `json:"metric" db:"metric"`
	Operator        string          `json:"operator" db:"operator"`
	Threshold       float64         `json:"threshold" db:"threshold"`
	WindowSeconds   int             `json:"window_seconds" db:"window_seconds"`
	Action          string          `json:"action" db:"action"`
	ActionParams    json.RawMessage `json:"action_params" db:"action_params"`
	CooldownSeconds int             `json:"cooldown_seconds" db:"cooldown_seconds"`
	Priority        int             `json:"priority" db:"priority"`
	Enabled         bool            `json:"enabled" db:"enabled"`
	CreatedAt       time.Time       `json:"created_at" db:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at" db:"updated_at"`
}

// Decision records a governance action taken by an agent.
type Decision struct {
	ID             string          `json:"id" db:"id"`
	OrganizationID string          `json:"organization_id" db:"organization_id"`
	ISP            ISP             `json:"isp" db:"isp"`
	AgentType      AgentType       `json:"agent_type" db:"agent_type"`
	RuleID         *string         `json:"rule_id,omitempty" db:"rule_id"`
	SignalValues   json.RawMessage `json:"signal_values" db:"signal_values"`
	ActionTaken    string          `json:"action_taken" db:"action_taken"`
	ActionParams   json.RawMessage `json:"action_params" db:"action_params"`
	TargetType     string          `json:"target_type" db:"target_type"`
	TargetValue    string          `json:"target_value" db:"target_value"`
	Result         string          `json:"result" db:"result"`
	RevertedAt     *time.Time      `json:"reverted_at,omitempty" db:"reverted_at"`
	RevertReason   string          `json:"revert_reason,omitempty" db:"revert_reason"`
	S3DecisionKey  string          `json:"s3_decision_key,omitempty" db:"s3_decision_key"`
	CreatedAt      time.Time       `json:"created_at" db:"created_at"`
}

// AgentState represents the persisted state of one agent instance.
type AgentState struct {
	ID             string          `json:"id" db:"id"`
	OrganizationID string          `json:"organization_id" db:"organization_id"`
	ISP            ISP             `json:"isp" db:"isp"`
	AgentType      AgentType       `json:"agent_type" db:"agent_type"`
	Status         AgentStatus     `json:"status" db:"status"`
	LastEvalAt     *time.Time      `json:"last_eval_at,omitempty" db:"last_eval_at"`
	DecisionsCount int             `json:"decisions_count" db:"decisions_count"`
	CurrentActions json.RawMessage `json:"current_actions" db:"current_actions"`
	ErrorMessage   string          `json:"error_message,omitempty" db:"error_message"`
	S3StateKey     string          `json:"s3_state_key,omitempty" db:"s3_state_key"`
	CreatedAt      time.Time       `json:"created_at" db:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at" db:"updated_at"`
}

// Suppression represents one ISP-scoped suppressed email address.
type Suppression struct {
	ID             string    `json:"id" db:"id"`
	OrganizationID string    `json:"organization_id" db:"organization_id"`
	Email          string    `json:"email" db:"email"`
	ISP            ISP       `json:"isp" db:"isp"`
	Reason         string    `json:"reason" db:"reason"`
	DSNCode        string    `json:"dsn_code,omitempty" db:"dsn_code"`
	DSNDiagnostic  string    `json:"dsn_diagnostic,omitempty" db:"dsn_diagnostic"`
	SourceIP       string    `json:"source_ip,omitempty" db:"source_ip"`
	SourceVMTA     string    `json:"source_vmta,omitempty" db:"source_vmta"`
	CampaignID     string    `json:"campaign_id,omitempty" db:"campaign_id"`
	SuppressedAt   time.Time `json:"suppressed_at" db:"suppressed_at"`
	CreatedAt      time.Time `json:"created_at" db:"created_at"`
}

// SuppressionStats holds aggregated suppression statistics for one ISP.
type SuppressionStats struct {
	ISP            ISP                 `json:"isp"`
	TotalCount     int64               `json:"total_count"`
	TodayCount     int64               `json:"today_count"`
	Last24hCount   int64               `json:"last_24h_count"`
	Last1hCount    int64               `json:"last_1h_count"`
	TopReasons     []ReasonCount       `json:"top_reasons"`
	TopCampaigns   []CampaignCount     `json:"top_campaigns"`
	VelocityPerMin float64             `json:"velocity_per_min"`
}

// ReasonCount is a reason + count pair for aggregation.
type ReasonCount struct {
	Reason string `json:"reason"`
	Count  int64  `json:"count"`
}

// CampaignCount is a campaign_id + count pair for aggregation.
type CampaignCount struct {
	CampaignID string `json:"campaign_id"`
	Count      int64  `json:"count"`
}

// SuppressionCheckResult is the response for a pre-send check.
type SuppressionCheckResult struct {
	Email       string     `json:"email"`
	ISP         ISP        `json:"isp"`
	Suppressed  bool       `json:"suppressed"`
	Reason      string     `json:"reason,omitempty"`
	SuppressedAt *time.Time `json:"suppressed_at,omitempty"`
}

// AccountingRecord represents a parsed PMTA accounting record.
// Supports both the native PMTA field names and the forwarder script's
// normalized field names via custom UnmarshalJSON.
type AccountingRecord struct {
	Type         string `json:"type"`
	Recipient    string `json:"recipient"`
	Sender       string `json:"sender"`
	BounceCat    string `json:"bounce_cat"`
	DSNStatus    string `json:"dsn_status"`
	DSNDiag      string `json:"dsn_diag"`
	SourceIP     string `json:"source_ip"`
	VMTA         string `json:"vmta"`
	Pool         string `json:"pool"`
	Domain       string `json:"domain"`
	DestIP       string `json:"dest_ip"`
	TLS          string `json:"tls"`
	Size         int64  `json:"size"`
	DeliveryTime string `json:"time_logged"`
	FeedbackType string `json:"feedback_type"`
	JobID        string `json:"job_id"`
}

// UnmarshalJSON handles both forwarder-style and legacy field names.
func (r *AccountingRecord) UnmarshalJSON(data []byte) error {
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	str := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := raw[k]; ok {
				if s, ok := v.(string); ok && s != "" {
					return s
				}
			}
		}
		return ""
	}

	r.Type = str("type")
	r.Recipient = str("recipient", "rcpt")
	r.Sender = str("sender", "from", "orig")
	r.BounceCat = str("bounce_cat", "bounceCat")
	r.DSNStatus = str("dsn_status", "dsnStatus")
	r.DSNDiag = str("dsn_diag", "dsnDiag")
	r.SourceIP = str("source_ip", "vmtaIp", "dlvSourceIp")
	r.VMTA = str("vmta")
	r.Pool = str("pool", "jobPool", "vmtaPool")
	r.Domain = str("domain", "rcptDomain", "queue")
	r.DestIP = str("dest_ip", "dlvDestIp", "dlvDestinationIp")
	r.TLS = str("tls", "dlvTlsProtocol")
	r.DeliveryTime = str("time_logged", "dlvStamp", "timeLogged")
	r.FeedbackType = str("feedback_type", "fbType", "feedbackType")
	r.JobID = str("job_id", "jobId")

	if v, ok := raw["size"]; ok {
		switch s := v.(type) {
		case float64:
			r.Size = int64(s)
		case string:
			fmt.Sscanf(s, "%d", &r.Size)
		}
	}
	if r.Size == 0 {
		if v, ok := raw["msgSize"]; ok {
			if s, ok := v.(float64); ok {
				r.Size = int64(s)
			}
		}
	}

	// Extract domain from queue field (e.g., "gmail.com/mta1" -> "gmail.com")
	if r.Domain != "" {
		if idx := len(r.Domain); idx > 0 {
			for i, ch := range r.Domain {
				if ch == '/' {
					r.Domain = r.Domain[:i]
					break
				}
			}
		}
	}

	// Extract domain from recipient if domain still empty
	if r.Domain == "" && r.Recipient != "" {
		for i := len(r.Recipient) - 1; i >= 0; i-- {
			if r.Recipient[i] == '@' {
				r.Domain = r.Recipient[i+1:]
				break
			}
		}
	}

	return nil
}

// ISPHealthSummary is the overview for one ISP on the global dashboard.
type ISPHealthSummary struct {
	ISP              ISP           `json:"isp"`
	DisplayName      string        `json:"display_name"`
	HealthScore      float64       `json:"health_score"`
	AgentStates      []AgentState  `json:"agent_states"`
	ActiveAgents     int           `json:"active_agents"`
	RecentDecisions  int           `json:"recent_decisions"`
	BounceRate       float64       `json:"bounce_rate"`
	DeferralRate     float64       `json:"deferral_rate"`
	ComplaintRate    float64       `json:"complaint_rate"`
	SuppressionCount int64         `json:"suppression_count"`
	HasEmergency     bool          `json:"has_emergency"`
	PoolName         string        `json:"pool_name"`
	IPCount          int           `json:"ip_count"`
}

// EngineOverview is the full global dashboard response.
type EngineOverview struct {
	ISPs             []ISPHealthSummary `json:"isps"`
	TotalAgents      int                `json:"total_agents"`
	ActiveAgents     int                `json:"active_agents"`
	RecentDecisions  []Decision         `json:"recent_decisions"`
	TotalSuppressions int64             `json:"total_suppressions"`
}

// IncidentReport is a full emergency incident report.
type IncidentReport struct {
	ID              string          `json:"id"`
	ISP             ISP             `json:"isp"`
	Trigger         string          `json:"trigger"`
	TriggerMetrics  json.RawMessage `json:"trigger_metrics"`
	AffectedIPs     []string        `json:"affected_ips"`
	AffectedDomains []string        `json:"affected_domains"`
	StartedAt       time.Time       `json:"started_at"`
	DetectedAt      time.Time       `json:"detected_at"`
	ResolvedAt      *time.Time      `json:"resolved_at,omitempty"`
	DSNSamples      []string        `json:"dsn_samples"`
	ActionsTaken    []string        `json:"actions_taken"`
	Status          string          `json:"status"`
}

// ---------------------------------------------------------------------------
// Conviction System — Binary Verdict Memory
// ---------------------------------------------------------------------------

// Verdict is the binary outcome of every agent memory: deterministic yes or no.
type Verdict string

const (
	VerdictWill Verdict = "will"
	VerdictWont Verdict = "wont"
)

// Conviction is a single micro-memory: a specific moment where an agent made
// a deterministic binary decision, stored with full contextual detail.
// Agents don't generalize. They accumulate specific observations.
// Pattern recognition happens at query time, not storage time.
type Conviction struct {
	ID         string       `json:"id"`
	AgentType  AgentType    `json:"agent_type"`
	ISP        ISP          `json:"isp"`
	Verdict    Verdict      `json:"verdict"`
	Statement  string       `json:"statement"`
	Context    MicroContext `json:"context"`
	Confidence float64      `json:"confidence"`
	Corroborations int      `json:"corroborations"`
	CreatedAt  time.Time    `json:"created_at"`
	LastSeenAt time.Time    `json:"last_seen_at"`
	Outcome    *ConvictionOutcome `json:"outcome,omitempty"`
}

// MicroContext captures the exact circumstances of a conviction.
// Every field is optional — agents populate what's relevant to their domain.
type MicroContext struct {
	// Temporal — the exact moment, not a generalization
	Date        string `json:"date,omitempty"`
	DayOfWeek   string `json:"day_of_week,omitempty"`
	HourUTC     int    `json:"hour_utc,omitempty"`
	IsHoliday   bool   `json:"is_holiday,omitempty"`
	HolidayName string `json:"holiday_name,omitempty"`

	// Infrastructure
	IP   string `json:"ip,omitempty"`
	VMTA string `json:"vmta,omitempty"`
	Domain string `json:"domain,omitempty"`
	Pool   string `json:"pool,omitempty"`

	// Volume — what was attempted
	AttemptedVolume int `json:"attempted_volume,omitempty"`
	AttemptedRate   int `json:"attempted_rate,omitempty"`

	// Signal metrics at the moment of the verdict
	BounceRate     float64 `json:"bounce_rate,omitempty"`
	DeferralRate   float64 `json:"deferral_rate,omitempty"`
	ComplaintRate  float64 `json:"complaint_rate,omitempty"`
	AcceptanceRate float64 `json:"acceptance_rate,omitempty"`
	TrueOpenRate   float64 `json:"true_open_rate,omitempty"`

	// Granular counts within the observation window
	DeferralCount  int `json:"deferral_count,omitempty"`
	AcceptedCount  int `json:"accepted_count,omitempty"`
	BounceCount    int `json:"bounce_count,omitempty"`
	ComplaintCount int `json:"complaint_count,omitempty"`

	// ISP response detail
	DSNCodes       []string `json:"dsn_codes,omitempty"`
	DSNDiagnostics []string `json:"dsn_diagnostics,omitempty"`

	// Suppression-specific
	Email      string `json:"email,omitempty"`
	CampaignID string `json:"campaign_id,omitempty"`
	Reason     string `json:"reason,omitempty"`

	// Pool-specific
	IPScore       float64 `json:"ip_score,omitempty"`
	FromPool      string  `json:"from_pool,omitempty"`
	ToPool        string  `json:"to_pool,omitempty"`

	// Warmup-specific
	WarmupDay   int `json:"warmup_day,omitempty"`
	DailyVolume int `json:"daily_volume,omitempty"`

	// Throttle-specific
	EffectiveRate  int     `json:"effective_rate,omitempty"`
	BackoffStep    int     `json:"backoff_step,omitempty"`
	RecoveryTimeMin float64 `json:"recovery_time_min,omitempty"`
	PriorRateAdj   float64 `json:"prior_rate_adj,omitempty"`
}

// ConvictionOutcome records what actually happened after the verdict was applied.
// This closes the feedback loop — did the conviction lead to a good result?
type ConvictionOutcome struct {
	Success    bool               `json:"success"`
	Metrics    map[string]float64 `json:"metrics,omitempty"`
	ObservedAt time.Time          `json:"observed_at"`
	Notes      string             `json:"notes,omitempty"`
}

// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Campaign Wizard — PMTA-native campaign intelligence types
// ---------------------------------------------------------------------------

// ISPReadiness summarizes one ISP's readiness for a campaign deployment.
type ISPReadiness struct {
	ISP              ISP         `json:"isp"`
	DisplayName      string      `json:"display_name"`
	HealthScore      float64     `json:"health_score"`
	Status           string      `json:"status"` // ready, caution, blocked
	ActiveAgents     int         `json:"active_agents"`
	TotalAgents      int         `json:"total_agents"`
	BounceRate       float64     `json:"bounce_rate"`
	DeferralRate     float64     `json:"deferral_rate"`
	ComplaintRate    float64     `json:"complaint_rate"`
	WarmupIPs        int         `json:"warmup_ips"`
	ActiveIPs        int         `json:"active_ips"`
	QuarantinedIPs   int         `json:"quarantined_ips"`
	MaxDailyCapacity int         `json:"max_daily_capacity"`
	MaxHourlyRate    int         `json:"max_hourly_rate"`
	PoolName         string      `json:"pool_name"`
	HasEmergency     bool        `json:"has_emergency"`
	Warnings         []string    `json:"warnings"`
}

// CampaignReadinessResponse is returned by the campaign-readiness endpoint.
type CampaignReadinessResponse struct {
	ISPs          []ISPReadiness `json:"isps"`
	TotalCapacity int            `json:"total_capacity"`
	OverallStatus string         `json:"overall_status"` // ready, caution, blocked
}

// CampaignIntelRequest is the input for the campaign-intel endpoint.
type CampaignIntelRequest struct {
	TargetISPs     []ISP          `json:"target_isps"`
	AudiencePerISP map[string]int `json:"audience_per_isp"`
	SendDay        string         `json:"send_day"`
	SendHour       int            `json:"send_hour"`
}

// ISPIntel is the intelligence report for one ISP in a campaign context.
type ISPIntel struct {
	ISP                ISP              `json:"isp"`
	DisplayName        string           `json:"display_name"`
	ThroughputCapacity ThroughputInfo   `json:"throughput"`
	WarmupSummary      WarmupSummary    `json:"warmup_summary"`
	ConvictionSummary  ConvictionIntel  `json:"conviction_summary"`
	ActiveWarnings     []string         `json:"active_warnings"`
	Strategy           string           `json:"strategy"`
}

// ThroughputInfo describes sending capacity for an ISP.
type ThroughputInfo struct {
	MaxMsgRate       int  `json:"max_msg_rate"`
	ActiveIPs        int  `json:"active_ips"`
	MaxDailyCapacity int  `json:"max_daily_capacity"`
	MaxHourlyRate    int  `json:"max_hourly_rate"`
	AudienceSize     int  `json:"audience_size"`
	CanSendInOnePass bool `json:"can_send_in_one_pass"`
	EstimatedHours   int  `json:"estimated_hours"`
	Status           string `json:"status"` // green, yellow, red
}

// WarmupSummary describes the warmup state of IPs for an ISP pool.
type WarmupSummary struct {
	TotalIPs     int    `json:"total_ips"`
	WarmedIPs    int    `json:"warmed_ips"`
	WarmingIPs   int    `json:"warming_ips"`
	PausedIPs    int    `json:"paused_ips"`
	AvgWarmupDay int    `json:"avg_warmup_day"`
	DailyLimit   int    `json:"daily_limit"`
	Status       string `json:"status"` // established, ramping, early
}

// ConvictionIntel is a simplified recall synthesis for the campaign wizard.
type ConvictionIntel struct {
	DominantVerdict Verdict  `json:"dominant_verdict"`
	Confidence      float64  `json:"confidence"`
	WillCount       int      `json:"will_count"`
	WontCount       int      `json:"wont_count"`
	KeyObservations []string `json:"key_observations"`
	RiskFactors     []string `json:"risk_factors"`
}

// CampaignIntelResponse is returned by the campaign-intel endpoint.
type CampaignIntelResponse struct {
	ISPs           []ISPIntel `json:"isps"`
	OverallStrategy string   `json:"overall_strategy"`
}

// SendingDomainInfo describes a PMTA sending domain with its infrastructure.
type SendingDomainInfo struct {
	Domain          string   `json:"domain"`
	DKIMConfigured  bool     `json:"dkim_configured"`
	SPFConfigured   bool     `json:"spf_configured"`
	DMARCConfigured bool     `json:"dmarc_configured"`
	PoolName        string   `json:"pool_name"`
	IPCount         int      `json:"ip_count"`
	IPs             []string `json:"ips"`
	ActiveIPs       int      `json:"active_ips"`
	WarmupIPs       int      `json:"warmup_ips"`
	ReputationScore float64  `json:"reputation_score"`
	Status          string   `json:"status"` // active, degraded, inactive
}

// PMTACampaignInput is the deploy payload for creating a PMTA-routed campaign.
type PMTACampaignInput struct {
	Name              string           `json:"name"`
	TargetISPs        []ISP            `json:"target_isps"`
	SendingDomain     string           `json:"sending_domain"`
	Variants          []ContentVariant `json:"variants"`
	InclusionSegments []string         `json:"inclusion_segments"`
	InclusionLists    []string         `json:"inclusion_lists"`
	ExclusionSegments []string         `json:"exclusion_segments"`
	ExclusionLists    []string         `json:"exclusion_lists"`
	SendDays          []string         `json:"send_days"`
	SendHour          int              `json:"send_hour"`
	Timezone          string           `json:"timezone"`
	ThrottleStrategy  string           `json:"throttle_strategy"`
}

// ContentVariant represents one A/B variant of campaign content.
type ContentVariant struct {
	VariantName  string  `json:"variant_name"` // A, B, C, D
	FromName     string  `json:"from_name"`
	Subject      string  `json:"subject"`
	HTMLContent  string  `json:"html_content"`
	SplitPercent float64 `json:"split_percent"`
}

// PMTACampaignResult is returned after deploying a PMTA campaign.
type PMTACampaignResult struct {
	CampaignID    string   `json:"campaign_id"`
	Name          string   `json:"name"`
	Status        string   `json:"status"`
	TargetISPs    []ISP    `json:"target_isps"`
	TotalAudience int      `json:"total_audience"`
	VariantCount  int      `json:"variant_count"`
	AgentIDs      []string `json:"agent_ids"`
}

// AudienceEstimateRequest is the input for audience estimation with ISP breakdown.
type AudienceEstimateRequest struct {
	SegmentIDs          []string `json:"segment_ids"`
	ListIDs             []string `json:"list_ids"`
	SuppressionListIDs  []string `json:"suppression_list_ids"`
	SuppressionSegments []string `json:"suppression_segments"`
	TargetISPs          []ISP    `json:"target_isps"`
}

// AudienceEstimateResponse is the audience estimate with per-ISP breakdown.
type AudienceEstimateResponse struct {
	TotalRecipients    int            `json:"total_recipients"`
	AfterSuppressions  int            `json:"after_suppressions"`
	SuppressedCount    int            `json:"suppressed_count"`
	ISPBreakdown       map[string]int `json:"isp_breakdown"`
}

// WarmupDay defines one tier of the warmup schedule.
type WarmupDay struct {
	Day    int `json:"day"`
	Volume int `json:"volume"`
}

// DefaultWarmupSchedule returns the conservative 30-day ramp.
func DefaultWarmupSchedule() []WarmupDay {
	return []WarmupDay{
		{1, 50}, {2, 50}, {3, 100}, {4, 100},
		{5, 200}, {6, 200}, {7, 400}, {8, 400},
		{9, 800}, {10, 800}, {11, 1500}, {12, 1500},
		{13, 3000}, {14, 3000}, {15, 6000}, {16, 6000},
		{17, 12000}, {18, 12000}, {19, 20000}, {20, 20000},
		{21, 30000}, {22, 30000}, {23, 30000}, {24, 30000},
		{25, 40000}, {26, 40000}, {27, 40000}, {28, 40000},
		{29, 50000}, {30, 50000},
	}
}
