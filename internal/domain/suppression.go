package domain

import "time"

// SuppressionReason enumerates why an email was suppressed.
type SuppressionReason string

const (
	ReasonHardBounce    SuppressionReason = "hard_bounce"
	ReasonSoftBounce    SuppressionReason = "soft_bounce"
	ReasonComplaint     SuppressionReason = "spam_complaint"
	ReasonUnsubscribe   SuppressionReason = "unsubscribe"
	ReasonInactive      SuppressionReason = "inactive"
	ReasonManual        SuppressionReason = "manual"
	ReasonRoleBased     SuppressionReason = "role_based"
	ReasonInvalidSyntax SuppressionReason = "invalid_syntax"
)

// SuppressionSource indicates where the suppression signal originated.
type SuppressionSource string

const (
	SourcePMTABounce SuppressionSource = "pmta_bounce"
	SourcePMTAFBL    SuppressionSource = "pmta_fbl"
	SourceFBLReport  SuppressionSource = "fbl_report"
	SourceESPWebhook SuppressionSource = "esp_webhook"
	SourceTracking   SuppressionSource = "tracking_unsubscribe"
	SourceManual     SuppressionSource = "manual"
	SourceImport     SuppressionSource = "import"
)

// Suppression represents a single entry in the global suppression list.
type Suppression struct {
	ID             string            `json:"id" db:"id"`
	OrganizationID string            `json:"organization_id" db:"organization_id"`
	Email          string            `json:"email" db:"email"`
	MD5Hash        string            `json:"md5_hash" db:"md5_hash"`
	Reason         SuppressionReason `json:"reason" db:"reason"`
	Source         SuppressionSource `json:"source" db:"source"`
	ISP            string            `json:"isp,omitempty" db:"isp"`
	DSNCode        string            `json:"dsn_code,omitempty" db:"dsn_code"`
	DSNDiag        string            `json:"dsn_diag,omitempty" db:"dsn_diag"`
	SourceIP       string            `json:"source_ip,omitempty" db:"source_ip"`
	CampaignID     string            `json:"campaign_id,omitempty" db:"campaign_id"`
	CreatedAt      time.Time         `json:"created_at" db:"created_at"`
}
