package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// SlackNotifier posts alerts to Slack via the Web API chat.postMessage method
// using a bot token (xoxb-…). It deliberately uses the standard library only.
type SlackNotifier struct {
	token   string
	channel string
	http    *http.Client
}

// NewSlackNotifier constructs a Slack notifier. token is a bot token with the
// chat:write scope; channel is a channel id (e.g. C0123) or name (e.g. #alerts).
func NewSlackNotifier(token, channel string) *SlackNotifier {
	return &SlackNotifier{
		token:   token,
		channel: channel,
		http:    &http.Client{Timeout: 8 * time.Second},
	}
}

func (s *SlackNotifier) Name() string { return "slack" }

// Notify posts a formatted message. Errors (network, auth, rate-limit) are
// returned for logging but never panic. Title is bolded; body is appended.
func (s *SlackNotifier) Notify(title, body string) error {
	payload := map[string]any{
		"channel": s.channel,
		"text":    fmt.Sprintf("*%s*\n%s", title, body),
	}
	buf, _ := json.Marshal(payload)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://slack.com/api/chat.postMessage", bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+s.token)

	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	// Slack returns 200 with {"ok":false,"error":"..."} on logical failures.
	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(raw, &out)
	if !out.OK {
		return fmt.Errorf("slack chat.postMessage failed: status=%d error=%q", resp.StatusCode, out.Error)
	}
	return nil
}

// NotifyBlocks posts a Block Kit message (house style) with a plain-text
// fallback via chat.postMessage. Implements BlockNotifier. The `text` field is
// the notification/old-client fallback; `blocks` is the rich rendering.
func (s *SlackNotifier) NotifyBlocks(blocks []any, fallback string) error {
	payload := map[string]any{
		"channel":       s.channel,
		"text":          fallback,
		"blocks":        blocks,
		"unfurl_links":  false,
		"unfurl_media":  false,
	}
	buf, _ := json.Marshal(payload)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://slack.com/api/chat.postMessage", bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+s.token)

	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(raw, &out)
	if !out.OK {
		return fmt.Errorf("slack chat.postMessage (blocks) failed: status=%d error=%q", resp.StatusCode, out.Error)
	}
	return nil
}

// SlackWebhookNotifier posts alerts to a Slack Incoming Webhook URL
// (https://hooks.slack.com/services/...). The destination channel is fixed by
// the webhook itself, so no channel or token is required. This is the simplest
// transport and is preferred when SLACK_WEBHOOK_URL is set.
type SlackWebhookNotifier struct {
	url  string
	http *http.Client
}

// NewSlackWebhookNotifier constructs a webhook-based notifier. url is a Slack
// Incoming Webhook URL.
func NewSlackWebhookNotifier(url string) *SlackWebhookNotifier {
	return &SlackWebhookNotifier{
		url:  url,
		http: &http.Client{Timeout: 8 * time.Second},
	}
}

func (s *SlackWebhookNotifier) Name() string { return "slack-webhook" }

// Notify posts a formatted message to the webhook. A Slack Incoming Webhook
// returns a plain "ok" body (HTTP 200) on success; any non-2xx is an error.
func (s *SlackWebhookNotifier) Notify(title, body string) error {
	payload := map[string]any{
		"text": fmt.Sprintf("*%s*\n%s", title, body),
	}
	buf, _ := json.Marshal(payload)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("slack webhook failed: status=%d body=%q", resp.StatusCode, string(raw))
	}
	return nil
}

// NotifyBlocks posts a Block Kit message to the webhook with a plain-text
// fallback. Implements BlockNotifier.
func (s *SlackWebhookNotifier) NotifyBlocks(blocks []any, fallback string) error {
	payload := map[string]any{"text": fallback, "blocks": blocks}
	buf, _ := json.Marshal(payload)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("slack webhook (blocks) failed: status=%d body=%q", resp.StatusCode, string(raw))
	}
	return nil
}
