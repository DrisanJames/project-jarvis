// Package worker contains the send worker pool and ESP sender adapters.
//
// ESP adapters are split into individual files:
//   - esp_sparkpost.go: SparkPost Transmissions API
//   - esp_ses.go:       AWS SES v2
//   - esp_mailgun.go:   Mailgun Messages API (batch-capable)
//   - esp_sendgrid.go:  SendGrid v3 Mail Send (batch-capable)
//   - esp_pmta.go:      PowerMTA via direct SMTP with VMTA routing
//   - esp_profile.go:   Database-driven profile resolver that delegates to the above
package worker

import "context"

// BatchSender extends ESPSender for adapters that support multi-recipient
// sends in a single API call. Mailgun and SendGrid implement this.
type BatchSender interface {
	SendBatch(ctx context.Context, messages []EmailMessage) (*BatchSendResult, error)
	MaxBatchSize() int
}

// BatchSendResult holds aggregate results from a batch send operation.
type BatchSendResult struct {
	TransmissionID string
	Accepted       int
	Rejected       int
	Results        []SendResult
	Errors         []error
}
