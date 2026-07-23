package emailoversight

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	DefaultAPIURL   = "https://api.emailoversight.com/api/EmailValidation"
	ResultVerified  = "Verified"
	ResultCatchAll  = "Catch All"
	ResultUnknown   = "Unknown"
	maxRetries      = 3
	retryBaseDelay  = 500 * time.Millisecond

	// ResultReject is the Result value of EO's account-suppression response
	// shape: {"result":"REJECT","reason":"Suppressed Email Address",...}.
	// Note the lowercase keys — a different shape from normal validations.
	ResultReject = "REJECT"

	// ResultIDRejected is a synthetic ResultId assigned to REJECT responses,
	// which carry no ResultId of their own (JSON decodes them to 0, which
	// callers historically confused with EO's retryable result 0). Negative
	// so it can never collide with a real EO result id.
	ResultIDRejected = -1
)

type ValidationRequest struct {
	ListID int    `json:"ListId"`
	Email  string `json:"Email"`
}

type ValidationResponse struct {
	ListID   int    `json:"ListId"`
	Email    string `json:"Email"`
	ResultID int    `json:"ResultId"`
	Result   string `json:"Result"`
	// Reason is populated only by the REJECT shape (lowercase "reason" key;
	// Go's JSON decoder matches struct tags case-insensitively, so both
	// "Reason" and "reason" land here — same for Result/result above).
	Reason string `json:"Reason"`
}

// IsReject reports whether the response is EO's account-suppression REJECT
// shape. This is a terminal verdict (e.g. reason "Suppressed Email Address"),
// never retryable.
func (r *ValidationResponse) IsReject() bool {
	return strings.EqualFold(strings.TrimSpace(r.Result), ResultReject)
}

type BatchResult struct {
	Email   string
	Result  string
	IsValid bool
	Err     error
}

// ValidResults returns the set of EO result strings we treat as deliverable.
var ValidResults = map[string]bool{
	"Verified": true,
}

type Client struct {
	apiURL      string
	apiToken    string
	listID      int
	concurrency int
	httpClient  *http.Client

	totalCalls    atomic.Int64
	validCount    atomic.Int64
	invalidCount  atomic.Int64
}

func New(apiToken string, listID, concurrency int) *Client {
	if concurrency < 1 {
		concurrency = 10
	}
	return &Client{
		apiURL:      DefaultAPIURL,
		apiToken:    apiToken,
		listID:      listID,
		concurrency: concurrency,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (c *Client) Stats() (total, valid, invalid int64) {
	return c.totalCalls.Load(), c.validCount.Load(), c.invalidCount.Load()
}

// Validate sends a single email validation request with retries.
func (c *Client) Validate(ctx context.Context, email string) (*ValidationResponse, error) {
	body, _ := json.Marshal(ValidationRequest{ListID: c.listID, Email: email})

	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if attempt > 0 {
			time.Sleep(retryBaseDelay * time.Duration(1<<(attempt-1)))
		}

		req, err := http.NewRequestWithContext(ctx, "POST", c.apiURL, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("ApiToken", c.apiToken)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
			continue
		}
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
		}

		var vr ValidationResponse
		if err := json.Unmarshal(respBody, &vr); err != nil {
			return nil, fmt.Errorf("decode response: %w", err)
		}
		if vr.IsReject() {
			// REJECT responses carry no ResultId (decodes to 0, which callers
			// would misread as EO's retryable result 0). Stamp the synthetic
			// terminal id so consumers see a distinct, final outcome.
			vr.ResultID = ResultIDRejected
		}

		c.totalCalls.Add(1)
		if ValidResults[vr.Result] {
			c.validCount.Add(1)
		} else {
			c.invalidCount.Add(1)
		}
		return &vr, nil
	}
	return nil, fmt.Errorf("all %d retries exhausted: %w", maxRetries, lastErr)
}

// ValidateBatch validates a slice of emails concurrently, respecting the configured concurrency limit.
func (c *Client) ValidateBatch(ctx context.Context, emails []string) []BatchResult {
	results := make([]BatchResult, len(emails))
	sem := make(chan struct{}, c.concurrency)
	var wg sync.WaitGroup

	for i, email := range emails {
		wg.Add(1)
		go func(idx int, addr string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			resp, err := c.Validate(ctx, addr)
			if err != nil {
				results[idx] = BatchResult{Email: addr, Err: err}
				return
			}
			results[idx] = BatchResult{
				Email:   addr,
				Result:  resp.Result,
				IsValid: ValidResults[resp.Result],
			}
			time.Sleep(200 * time.Millisecond)
		}(i, email)
	}

	wg.Wait()
	var batchValid, batchInvalid, batchErr int
	for _, r := range results {
		if r.Err != nil {
			batchErr++
		} else if r.IsValid {
			batchValid++
		} else {
			batchInvalid++
		}
	}
	log.Printf("[EO] batch done: %d emails — valid=%d invalid=%d errors=%d",
		len(emails), batchValid, batchInvalid, batchErr)
	return results
}
