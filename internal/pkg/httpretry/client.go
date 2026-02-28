// Package httpretry provides an HTTP client with automatic retry logic,
// exponential backoff, and jitter for resilient external API calls.
package httpretry

import (
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"net/http"
	"time"
)

// HTTPDoer is the interface for executing HTTP requests.
// Both *http.Client and *RetryClient satisfy this interface.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// RetryClient wraps an HTTPDoer with retry logic using exponential backoff and jitter.
type RetryClient struct {
	client     HTTPDoer
	maxRetries int
	baseDelay  time.Duration
	maxDelay   time.Duration
}

// NewRetryClient creates a new RetryClient that wraps the given HTTPDoer.
// If client is nil, a default http.Client with 30s timeout is used.
// maxRetries is the number of retry attempts after the initial request (default 3).
func NewRetryClient(client HTTPDoer, maxRetries int) *RetryClient {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	if maxRetries <= 0 {
		maxRetries = 3
	}
	return &RetryClient{
		client:     client,
		maxRetries: maxRetries,
		baseDelay:  1 * time.Second,
		maxDelay:   30 * time.Second,
	}
}

// Do executes the HTTP request with retry logic.
// It retries on retryable status codes (429, 500, 502, 503, 504) and
// transient network/timeout errors. It does NOT retry on client errors
// (400, 401, 403, 404) or context cancellation.
// On the final attempt, it returns the response as-is so the caller
// can inspect the status code and body.
func (rc *RetryClient) Do(req *http.Request) (*http.Response, error) {
	var lastErr error

	for attempt := 0; attempt <= rc.maxRetries; attempt++ {
		// Check if context is already canceled
		if req.Context().Err() != nil {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, req.Context().Err()
		}

		// Backoff before retry (skip on first attempt)
		if attempt > 0 {
			// Reset request body for retry if applicable
			if req.GetBody != nil {
				body, err := req.GetBody()
				if err != nil {
					return nil, fmt.Errorf("httpretry: failed to reset request body: %w", err)
				}
				req.Body = body
			}

			delay := rc.calculateDelay(attempt)
			log.Printf("httpretry: retry attempt %d/%d for %s %s%s (waiting %s)",
				attempt, rc.maxRetries, req.Method, req.URL.Host, req.URL.Path, delay)

			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-req.Context().Done():
				timer.Stop()
				if lastErr != nil {
					return nil, lastErr
				}
				return nil, req.Context().Err()
			}
		}

		resp, err := rc.client.Do(req)
		if err != nil {
			lastErr = err
			// If the context was canceled/expired, don't retry
			if req.Context().Err() != nil {
				return nil, err
			}
			// Network/connection/timeout error — retry
			continue
		}

		// Non-retryable status code — return immediately (success or client error)
		if !isRetryableStatus(resp.StatusCode) {
			return resp, nil
		}

		// If this is the last attempt, return the response as-is
		// so the caller can read the body and handle the error
		if attempt == rc.maxRetries {
			return resp, nil
		}

		// Retryable status code — drain body for connection reuse, then retry
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		lastErr = fmt.Errorf("httpretry: server returned retryable status %d", resp.StatusCode)
	}

	return nil, lastErr
}

// calculateDelay returns the backoff duration for the given retry attempt.
// Uses exponential backoff with full jitter: random(0, min(maxDelay, baseDelay * 2^(attempt-1))).
func (rc *RetryClient) calculateDelay(attempt int) time.Duration {
	// Exponential backoff: baseDelay * 2^(attempt-1)
	expDelay := float64(rc.baseDelay) * math.Pow(2, float64(attempt-1))

	// Cap at maxDelay
	if expDelay > float64(rc.maxDelay) {
		expDelay = float64(rc.maxDelay)
	}

	// Full jitter: random duration between 0 and the calculated delay
	jittered := time.Duration(rand.Float64() * expDelay)

	// Ensure a minimum delay of 100ms to avoid busy-looping
	if jittered < 100*time.Millisecond {
		jittered = 100 * time.Millisecond
	}

	return jittered
}

// isRetryableStatus returns true if the HTTP status code indicates a
// transient server error that should be retried.
// Retries: 429 (Too Many Requests), 500, 502, 503, 504.
// Does NOT retry: 400, 401, 403, 404, or any other client error.
func isRetryableStatus(statusCode int) bool {
	switch statusCode {
	case http.StatusTooManyRequests: // 429
		return true
	case http.StatusInternalServerError: // 500
		return true
	case http.StatusBadGateway: // 502
		return true
	case http.StatusServiceUnavailable: // 503
		return true
	case http.StatusGatewayTimeout: // 504
		return true
	default:
		return false
	}
}
