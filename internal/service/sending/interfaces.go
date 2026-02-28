// Package sending defines the interfaces for email delivery through various ESPs.
//
// Each ESP (SparkPost, SES, Mailgun, PMTA, etc.) implements the Sender interface.
// The worker pool uses the SenderFactory to resolve which Sender to use for
// a given sending profile.
package sending

import (
	"context"

	"github.com/ignite/sparkpost-monitor/internal/domain"
)

// Sender sends a single email through an ESP. Implementations must be
// safe for concurrent use.
type Sender interface {
	Send(ctx context.Context, msg *domain.EmailMessage) (*domain.SendResult, error)
}

// BatchSender extends Sender with batch delivery support. ESPs that support
// multi-recipient calls (SparkPost, SES, Mailgun) implement this.
type BatchSender interface {
	Sender
	SendBatch(ctx context.Context, messages []*domain.EmailMessage) ([]domain.SendResult, error)
	MaxBatchSize() int
}

// SenderFactory resolves a Sender for a given sending profile. This allows
// the worker pool to remain ESP-agnostic.
type SenderFactory interface {
	SenderFor(ctx context.Context, profileID string) (Sender, error)
}

// SuppressionChecker performs a pre-send suppression check. The send worker
// calls this before delivering each message.
type SuppressionChecker interface {
	IsSuppressed(email string) bool
}

// TrackingInjector modifies email HTML to add open pixels, click redirects,
// and unsubscribe links. Called by the send worker before delivery.
type TrackingInjector interface {
	InjectTracking(html, campaignID, subscriberID, emailID string) string
	GenerateUnsubscribeURL(campaignID, subscriberID string) string
	GenerateHeaders(campaignID, subscriberID string) map[string]string
}
