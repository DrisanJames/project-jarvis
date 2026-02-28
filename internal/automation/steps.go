package automation

import (
	"time"

	"github.com/google/uuid"
)

// Step is a single step in an automation flow.
type Step struct {
	Type       string `json:"type"`
	Template   string `json:"template,omitempty"`
	DelayHours int    `json:"delay_hours"`
	Check      string `json:"check,omitempty"`
	OnFalse    string `json:"on_false,omitempty"`
}

// Flow is the Go representation of an automation_flows row.
type Flow struct {
	ID             uuid.UUID `json:"id"`
	OrganizationID uuid.UUID `json:"organization_id"`
	Name           string    `json:"name"`
	Description    string    `json:"description"`
	TriggerEvent   string    `json:"trigger_event"`
	Steps          []Step    `json:"steps"`
	Status         string    `json:"status"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// Execution is the Go representation of an automation_executions row.
type Execution struct {
	ID           uuid.UUID  `json:"id"`
	FlowID       uuid.UUID  `json:"flow_id"`
	SubscriberID uuid.UUID  `json:"subscriber_id"`
	Email        string     `json:"email"`
	CurrentStep  int        `json:"current_step"`
	Status       string     `json:"status"`
	NextRunAt    *time.Time `json:"next_run_at"`
	CompletedAt  *time.Time `json:"completed_at"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// EmailSender is called by steps that send email. Implemented by the mailing service.
type EmailSender interface {
	SendTransactional(ctx interface{}, orgID string, to, subject, html string) error
}
