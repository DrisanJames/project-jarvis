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
	title    string
}

// NewSlackAlerter builds a Slack-backed alerter. title is the bold header on
// each posted message (e.g. "Campaign lateness"). A nil notifier yields a
// disabled alerter whose SendSMS returns an error (which callers log).
func NewSlackAlerter(n notify.Notifier, title string) *SlackAlerter {
	if title == "" {
		title = "Project Jarvis alert"
	}
	return &SlackAlerter{notifier: n, title: title}
}

// SendSMS satisfies SMSAlerter by posting body to Slack. The `ctx` and `to`
// arguments are unused (notify has its own timeout; the channel is fixed). On
// success it returns the sentinel id "slack" so callers that treat a non-empty
// id as "delivered" behave the same as with the Twilio SID.
func (s *SlackAlerter) SendSMS(_ context.Context, _ string, body string) (string, error) {
	if s == nil || s.notifier == nil {
		return "", fmt.Errorf("slack alerter: nil notifier")
	}
	if err := s.notifier.Notify(s.title, body); err != nil {
		return "", err
	}
	return "slack", nil
}
