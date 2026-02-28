package datanorm

import (
	"time"

	"github.com/google/uuid"
)

// Classification is the file type enum.
type Classification string

const (
	ClassMailable    Classification = "mailable"
	ClassSuppression Classification = "suppression"
	ClassWarmup      Classification = "warmup"
)

// ImportResult tracks the outcome of an import batch.
type ImportResult struct {
	FileKey        string
	Classification Classification
	RenamedKey     string
	TotalRows      int
	ImportedRows   int
	ErrorRows      int
	Duration       time.Duration
}

// Config holds normalizer configuration loaded from config.yaml.
type Config struct {
	Bucket     string
	Region     string
	AWSProfile string
	OrgID      string
	ListID     string
	Interval   time.Duration
}

// SubscriberEvent is the Go representation of a subscriber_events row.
type SubscriberEvent struct {
	ID         int64
	EmailHash  string
	EventType  string
	CampaignID *uuid.UUID
	VariantID  *uuid.UUID
	Source     string
	Metadata   map[string]interface{}
	EventAt    time.Time
}
