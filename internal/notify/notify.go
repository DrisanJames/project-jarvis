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

// BlockNotifier is an optional capability: a transport that can post Block Kit
// blocks (the house style — see render.go / docs/SLACK_COMMS_STANDARD.md) with a
// plain-text fallback. Notifiers that don't implement it fall back to Notify()
// via Deliver(). fallback is the notification text (and old-client mirror).
type BlockNotifier interface {
	NotifyBlocks(blocks []any, fallback string) error
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

// SlackChannelFromEnv builds a notifier bound to a specific Slack channel using
// the shared SLACK_BOT_TOKEN. channelEnv (optional) overrides defaultChannel at
// runtime — e.g. SlackChannelFromEnv("SLACK_WORKER_STALL_CHANNEL", "#worker-stall").
// Falls back to SLACK_WEBHOOK_URL (channel fixed by the webhook) when no bot
// token is set, then NoopNotifier. Used for the per-pager operational alert
// channels (campaign-lateness, outbox self-check, storage guard, worker-stall).
func SlackChannelFromEnv(channelEnv, defaultChannel string) Notifier {
	ch := defaultChannel
	if channelEnv != "" {
		if v := os.Getenv(channelEnv); v != "" {
			ch = v
		}
	}
	if token := os.Getenv("SLACK_BOT_TOKEN"); token != "" && ch != "" {
		return NewSlackNotifier(token, ch)
	}
	if webhook := os.Getenv("SLACK_WEBHOOK_URL"); webhook != "" {
		return NewSlackWebhookNotifier(webhook)
	}
	return NoopNotifier{}
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
