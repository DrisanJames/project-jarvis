// Package notify provides a small, dependency-light notification abstraction
// used for operational alerts (e.g. worker stalls). The concrete transport is
// selected at runtime from the environment so secrets never live in committed
// config. Transport precedence:
//
//  1. SLACK_WEBHOOK_URL — a Slack Incoming Webhook URL (channel fixed by the
//     webhook). Simplest; preferred when set.
//  2. SLACK_BOT_TOKEN (+ optional SLACK_ALERT_CHANNEL, default "#alerts") — a
//     Slack "xoxb-…" bot token using the chat.postMessage Web API.
//  3. neither set — alerts are logged only (NoopNotifier).
package notify

import (
	"log"
	"os"
)

// Notifier delivers a short operational alert. Implementations MUST be safe for
// concurrent use and MUST never block the caller for long — callers are workers
// on operational hot paths.
type Notifier interface {
	// Notify sends an alert. title is a short summary; body is detail. It
	// returns an error only to aid logging/tests; callers typically ignore it.
	Notify(title, body string) error
	// Name identifies the transport for logging.
	Name() string
}

// NoopNotifier logs alerts instead of sending them. Used when no external
// transport is configured.
type NoopNotifier struct{}

func (NoopNotifier) Notify(title, body string) error {
	log.Printf("[notify:noop] %s — %s", title, body)
	return nil
}

func (NoopNotifier) Name() string { return "noop" }

// FromEnv selects a transport per the precedence documented on the package:
// SLACK_WEBHOOK_URL > SLACK_BOT_TOKEN (+ SLACK_ALERT_CHANNEL) > NoopNotifier.
func FromEnv() Notifier {
	if webhook := os.Getenv("SLACK_WEBHOOK_URL"); webhook != "" {
		return NewSlackWebhookNotifier(webhook)
	}
	token := os.Getenv("SLACK_BOT_TOKEN")
	if token == "" {
		return NoopNotifier{}
	}
	channel := os.Getenv("SLACK_ALERT_CHANNEL")
	if channel == "" {
		channel = "#alerts"
	}
	return NewSlackNotifier(token, channel)
}

// ConversionsFromEnv selects the transport for revenue/conversion alerts, which
// the operator wants in a dedicated #conversions channel separate from the
// operational #alerts stream. Precedence:
//
//  1. SLACK_CONVERSIONS_WEBHOOK_URL — an Incoming Webhook bound to #conversions
//     (channel fixed by the webhook). Simplest; preferred when set.
//  2. SLACK_BOT_TOKEN (+ optional SLACK_CONVERSIONS_CHANNEL, default
//     "#conversions") — reuses the shared bot token but posts to the
//     conversions channel via chat.postMessage.
//  3. neither set — alerts are logged only (NoopNotifier).
func ConversionsFromEnv() Notifier {
	if webhook := os.Getenv("SLACK_CONVERSIONS_WEBHOOK_URL"); webhook != "" {
		return NewSlackWebhookNotifier(webhook)
	}
	token := os.Getenv("SLACK_BOT_TOKEN")
	if token == "" {
		return NoopNotifier{}
	}
	channel := os.Getenv("SLACK_CONVERSIONS_CHANNEL")
	if channel == "" {
		channel = "#conversions"
	}
	return NewSlackNotifier(token, channel)
}
