package worker

import (
	"context"
	"fmt"

	"github.com/ignite/sparkpost-monitor/internal/notify"
)

// SlackAlerter adapts a notify.Notifier to the SMSAlerter interface so the
// operational pagers (campaign-lateness monitor, outbox self-check, storage
// guard) deliver to Slack instead of Twilio SMS. The destination channel is
// fixed by the notifier; the `to` argument is ignored — these pagers are wired
// with a single pseudo-recipient so each event posts exactly once.
//
// It satisfies the same worker.SMSAlerter interface the Twilio client did, so
// the pagers' existing dedup/bookkeeping (e.g. late_alert_sent_at stamping on a
// non-empty returned id) keeps working unchanged.
type SlackAlerter struct {
	notifier notify.Notifier
	scope    string      // stable subsystem label → notify.Message.Scope
	tier     notify.Tier // severity → notify.Message.Tier
}

// NewSlackAlerter builds a Slack-backed alerter. The supplied title becomes the
// message Scope (e.g. "Campaign lateness") and the tier defaults to TierAlert,
// preserving the prior behaviour. A nil notifier yields a disabled alerter whose
// SendSMS returns an error (which callers log).
func NewSlackAlerter(n notify.Notifier, title string) *SlackAlerter {
	return NewSlackAlerterTiered(n, title, notify.TierAlert)
}

// NewSlackAlerterTiered builds a Slack-backed alerter with an explicit house-style
// scope and tier. scope is the stable subsystem label that feeds Message.Scope;
// tier is the severity. An empty scope defaults to "Project Jarvis alert".
func NewSlackAlerterTiered(n notify.Notifier, scope string, tier notify.Tier) *SlackAlerter {
	if scope == "" {
		scope = "Project Jarvis alert"
	}
	if tier == "" {
		tier = notify.TierAlert
	}
	return &SlackAlerter{notifier: n, scope: scope, tier: tier}
}

// SendSMS satisfies SMSAlerter by posting body to Slack via the house-style
// renderer. The `ctx` and `to` arguments are unused (notify has its own timeout;
// the channel is fixed). The monitor's body string becomes the message Headline.
// On success it returns the sentinel id "slack" so callers that treat a non-empty
// id as "delivered" behave the same as with the Twilio SID.
func (s *SlackAlerter) SendSMS(_ context.Context, _ string, body string) (string, error) {
	if s == nil || s.notifier == nil {
		return "", fmt.Errorf("slack alerter: nil notifier")
	}
	msg := notify.Message{Tier: s.tier, Scope: s.scope, Headline: body}
	if err := notify.Deliver(s.notifier, msg); err != nil {
		return "", err
	}
	return "slack", nil
}
