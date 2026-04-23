// Package twilio is a small dependency-free client for sending SMS
// via the Twilio REST API. It exists for operational alerts
// (e.g. campaign-lateness pager notifications) and intentionally
// implements the minimum surface area we need.
package twilio

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const defaultBaseURL = "https://api.twilio.com"

// Client sends SMS via Twilio's Messages REST API.
type Client struct {
	AccountSID string
	AuthToken  string
	FromNumber string

	BaseURL    string
	HTTPClient *http.Client
}

// NewClient constructs a Client with sensible defaults.
// Returns nil if any of AccountSID, AuthToken, or FromNumber is empty —
// callers should treat a nil client as "alerting disabled".
func NewClient(accountSID, authToken, fromNumber string) *Client {
	if strings.TrimSpace(accountSID) == "" ||
		strings.TrimSpace(authToken) == "" ||
		strings.TrimSpace(fromNumber) == "" {
		return nil
	}
	return &Client{
		AccountSID: accountSID,
		AuthToken:  authToken,
		FromNumber: fromNumber,
		BaseURL:    defaultBaseURL,
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// messageResponse is the subset of the Twilio Messages.json response
// we care about for operational purposes.
type messageResponse struct {
	SID          string `json:"sid"`
	Status       string `json:"status"`
	ErrorCode    any    `json:"error_code"`
	ErrorMessage string `json:"error_message"`
}

// SendSMS sends a single SMS. It returns the Twilio message SID on success.
//
// Errors fall into three buckets:
//  1. request build / transport errors (wrap context)
//  2. non-2xx HTTP responses (include status + body for operator debugging)
//  3. Twilio-reported send failures (error_code / error_message non-empty)
func (c *Client) SendSMS(ctx context.Context, to, body string) (string, error) {
	if c == nil {
		return "", fmt.Errorf("twilio: client is nil (alerting disabled)")
	}
	if strings.TrimSpace(to) == "" {
		return "", fmt.Errorf("twilio: empty To number")
	}
	if strings.TrimSpace(body) == "" {
		return "", fmt.Errorf("twilio: empty message body")
	}

	endpoint := fmt.Sprintf("%s/2010-04-01/Accounts/%s/Messages.json",
		strings.TrimRight(c.BaseURL, "/"), url.PathEscape(c.AccountSID))

	form := url.Values{}
	form.Set("To", to)
	form.Set("From", c.FromNumber)
	form.Set("Body", body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("twilio: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(c.AccountSID, c.AuthToken)

	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("twilio: post: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("twilio: http %d: %s", resp.StatusCode, truncate(string(raw), 512))
	}

	var mr messageResponse
	if err := json.Unmarshal(raw, &mr); err != nil {
		return "", fmt.Errorf("twilio: decode response: %w (body=%s)", err, truncate(string(raw), 256))
	}
	if mr.ErrorMessage != "" || (mr.ErrorCode != nil && mr.ErrorCode != float64(0) && mr.ErrorCode != "0") {
		return mr.SID, fmt.Errorf("twilio: api error code=%v msg=%s", mr.ErrorCode, mr.ErrorMessage)
	}
	return mr.SID, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
