// jarvis_types.go — Type definitions for the Jarvis autonomous campaign orchestrator.
package api

import (
	"database/sql"
	"sync"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/activation"
	"github.com/ignite/sparkpost-monitor/internal/edatasource"
	"github.com/ignite/sparkpost-monitor/internal/everflow"
)

type JarvisOrchestrator struct {
	db           *sql.DB
	mailingSvc   *MailingService
	mu           sync.Mutex
	campaign     *JarvisCampaign
	edsClient    *edatasource.Client              // eDataSource inbox placement monitoring
	yahooAgent   *activation.YahooActivationAgent  // Yahoo-specific intelligence
	efClient     *everflow.Client                  // Everflow conversion tracking & attribution
}

type JarvisCampaign struct {
	ID              string            `json:"id"`
	OrganizationID  string            `json:"organization_id"`
	OfferID         string            `json:"offer_id"`
	OfferName       string            `json:"offer_name"`
	Status          string            `json:"status"` // pending, running, paused, completed, failed
	StartedAt       *time.Time        `json:"started_at"`
	EndsAt          *time.Time        `json:"ends_at"`
	Recipients      []JarvisRecipient `json:"recipients"`
	Creatives       []JarvisCreative  `json:"creatives"`
	TrackingLink    string            `json:"tracking_link"`
	SuppressionID   string            `json:"suppression_list_id"`
	Log             []JarvisLogEntry  `json:"log"`
	Metrics         JarvisMetrics     `json:"metrics"`
	SendingProfiles map[string]string `json:"sending_profiles"` // ISP -> profile ID
	CurrentRound    int               `json:"current_round"`
	MaxRounds       int               `json:"max_rounds"`
	GoalConversions int               `json:"goal_conversions"`
	SendingDomain   string            `json:"sending_domain"`    // derived from sending profile from_email
	PrimaryProfile  string            `json:"primary_profile"`   // primary sending profile UUID
	SubjectLines    []string          `json:"subject_lines"`     // loaded from creatives or launch request
}

type JarvisRecipient struct {
	Email          string     `json:"email"`
	Domain         string     `json:"domain"`
	ISP            string     `json:"isp"`
	Suppressed     bool       `json:"suppressed"`
	Status         string     `json:"status"` // pending, sent, delivered, opened, clicked, converted, bounced, failed, spam_suspected
	LastSentAt     *time.Time `json:"last_sent_at"`
	LastOpenAt     *time.Time `json:"last_open_at"`
	LastClickAt    *time.Time `json:"last_click_at"`
	SendCount      int        `json:"send_count"`
	MessageIDs     []string   `json:"message_ids"`
	ESP            string     `json:"esp"`
	CreativeID     int        `json:"creative_id"`
	Subject        string     `json:"subject"`
	SpamSuspected  bool       `json:"spam_suspected"`   // true if inbox placement looks like spam for THIS recipient
	SpamReason     string     `json:"spam_reason,omitempty"`
}

type JarvisCreative struct {
	ID      int    `json:"id"`
	Name    string `json:"name"`
	Subject string `json:"subject"`
	HTML    string `json:"html"`
	Sends   int    `json:"sends"`
	Opens   int    `json:"opens"`
	Clicks  int    `json:"clicks"`
}

type JarvisLogEntry struct {
	Timestamp time.Time `json:"timestamp"`
	Level     string    `json:"level"` // info, decision, action, warning, error, milestone
	Component string    `json:"component"`
	Message   string    `json:"message"`
	Data      any       `json:"data,omitempty"`
}

type JarvisMetrics struct {
	TotalSent        int                       `json:"total_sent"`
	TotalDelivered   int                       `json:"total_delivered"`
	TotalOpens       int                       `json:"total_opens"`
	TotalClicks      int                       `json:"total_clicks"`
	TotalConversions int                       `json:"total_conversions"`
	TotalBounces     int                       `json:"total_bounces"`
	TotalRevenue     float64                   `json:"total_revenue"`
	OpenRate         float64                   `json:"open_rate"`
	ClickRate        float64                   `json:"click_rate"`
	ConversionRate   float64                   `json:"conversion_rate"`
	RevenuePerSend   float64                   `json:"revenue_per_send"`
	ISPMetrics       map[string]*ISPMetrics    `json:"isp_metrics,omitempty"`
}

// ISPMetrics tracks per-ISP deliverability intelligence
type ISPMetrics struct {
	ISP            string     `json:"isp"`
	Sent           int        `json:"sent"`
	Delivered      int        `json:"delivered"`
	Opens          int        `json:"opens"`
	Clicks         int        `json:"clicks"`
	Bounced        int        `json:"bounced"`
	InboxRate      float64    `json:"inbox_rate"`       // from eDataSource (0-100)
	SpamRate       float64    `json:"spam_rate"`        // from eDataSource (0-100)
	LastInboxCheck *time.Time `json:"last_inbox_check"`
	SpamDetected   bool       `json:"spam_detected"`    // true if inbox_rate < threshold (ISP-level flag, informational only)
}

// sparkPostEvent from the Events API
type sparkPostEvent struct {
	Type      string `json:"type"`
	MessageID string `json:"message_id"`
	Recipient string `json:"rcpt_to"`
	Reason    string `json:"reason"`
	Timestamp string `json:"timestamp"`
}

type JarvisPlan struct {
	PlanID          string               `json:"plan_id"`
	CreatedAt       time.Time            `json:"created_at"`
	Status          string               `json:"status"` // planned, executing, completed, failed
	Campaigns       []JarvisPlannedCampaign `json:"campaigns"`
	Strategy        JarvisStrategy       `json:"strategy"`
	OwnerAccounts   []string             `json:"owner_accounts"`
	TotalEmails     int                  `json:"total_emails"`
	SendDate        string               `json:"send_date"`
	Playbook        *JarvisPlaybook      `json:"playbook,omitempty"`
}

type JarvisPlannedCampaign struct {
	OfferID           string                  `json:"offer_id"`
	OfferName         string                  `json:"offer_name"`
	SuppressionListID string                  `json:"suppression_list_id"`
	SuppressionName   string                  `json:"suppression_name"`
	TrackingLink      string                  `json:"tracking_link"`
	DurationHours     int                     `json:"duration_hours"`
	GoalConversions   int                     `json:"goal_conversions"`
	Recipients        []JarvisPlannedRecipient `json:"recipients"`
	SubjectLines      []string                `json:"subject_lines"`
	Rationale         string                  `json:"rationale"`
	EstimatedSends    int                     `json:"estimated_sends"`
}

type JarvisPlannedRecipient struct {
	Email         string `json:"email"`
	Domain        string `json:"domain"`
	ISP           string `json:"isp"`
	OptimalHours  []int  `json:"optimal_hours"`
	STOSource     string `json:"sto_source"` // domain, isp, industry_default
	InboxStatus   string `json:"inbox_status"` // healthy, degraded, spam_suspected, unknown
	Strategy      string `json:"strategy"` // normal, cautious, aggressive_inbox_recovery
}

type JarvisStrategy struct {
	Objective        string            `json:"objective"`
	Approach         string            `json:"approach"`
	CadenceMinutes   int               `json:"cadence_minutes"`
	MaxRoundsPerCampaign int           `json:"max_rounds_per_campaign"`
	STOEnabled       bool              `json:"sto_enabled"`
	ISPStrategies    map[string]string `json:"isp_strategies"` // ISP → strategy description
	KnownIssues      []string          `json:"known_issues"`
	Mitigations      []string          `json:"mitigations"`
}

type JarvisPlaybook struct {
	PlaybookID    string            `json:"playbook_id"`
	Name          string            `json:"name"`
	Description   string            `json:"description"`
	CreatedAt     time.Time         `json:"created_at"`
	OfferCriteria map[string]string `json:"offer_criteria"` // how offers were selected
	STOProfile    map[string][]int  `json:"sto_profile"`    // domain → optimal hours used
	ISPRules      map[string]string `json:"isp_rules"`      // ISP → behavioral rules
	SendCadence   string            `json:"send_cadence"`
	SuccessMetrics []string         `json:"success_metrics"`
}

