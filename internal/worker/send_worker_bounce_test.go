package worker

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsTransportError_TrueForInfrastructureFailures(t *testing.T) {
	cases := []struct {
		name   string
		errMsg string
	}{
		{"connection refused", `PMTA API request to http://15.204.101.125:19099/api/inject/v1: dial tcp 15.204.101.125:19099: connect: connection refused`},
		{"i/o timeout", `PMTA API request to http://15.204.101.125:19099/api/inject/v1: dial tcp 15.204.101.125:19099: i/o timeout`},
		{"no such host", `PMTA API request to http://pmta.example.com:19099: dial tcp: lookup pmta.example.com: no such host`},
		{"no route to host", `dial tcp 15.204.101.125:19099: connect: no route to host`},
		{"network unreachable", `dial tcp 15.204.101.125:19099: connect: network is unreachable`},
		{"connection reset", `PMTA API request: read tcp: connection reset by peer`},
		{"connection timed out", `dial tcp 15.204.101.125:587: connect: connection timed out`},
		{"tls handshake failure", `PMTA API request: tls handshake timeout`},
		{"broken pipe", `write tcp: broken pipe`},
		{"context deadline", `context deadline exceeded`},
		{"context canceled", `context canceled`},
		{"eof", `PMTA API request: unexpected EOF`},
		{"marshal error", `marshal PMTA payload: json: unsupported type`},
		{"create request error", `create PMTA request: invalid URL`},
		{"pmta api wrapper", `PMTA API request to http://15.204.101.125:19099/api/inject/v1: Post "...": dial tcp ...`},
		{"no sender", `no sender configured for pmta`},
		{"dial tcp generic", `dial tcp 10.0.0.1:587: connect: connection refused`},
		// Production-observed PMTA HTTP bridge failures. These were
		// previously being written as phantom soft-bounces — a message
		// that PMTA later successfully delivered appearing in analytics
		// as a bounce.
		{"PMTA bridge 502 timeout", `PMTA API error (HTTP 502): {"status": "error", "detail": "Connection unexpectedly closed: timed out"}`},
		{"PMTA bridge 502 out of slots", `PMTA API error (HTTP 502): {"status": "error", "detail": "(421, b'mta-ht-gn1.mail.em.historythinking.com out of connection slots')"}`},
		{"PMTA bridge 503", `PMTA API error (HTTP 503): service temporarily unavailable`},
		{"PMTA bridge 504", `PMTA API error (HTTP 504): gateway timeout`},
		{"PMTA bridge 408", `PMTA API error (HTTP 408): request timeout`},
		{"PMTA bridge 429", `PMTA API error (HTTP 429): rate limited`},
		{"out of connection slots only", `(421, b'host out of connection slots')`},
		{"connection unexpectedly closed only", `Connection unexpectedly closed: timed out`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.True(t, isTransportError(tc.errMsg),
				"expected transport error for: %s", tc.errMsg)
		})
	}
}

func TestPmtaHTTPStatus(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{`PMTA API error (HTTP 502): {"status":"error"}`, 502},
		{`PMTA API error (HTTP 504): gateway timeout`, 504},
		{`PMTA API error (HTTP 400): bad request`, 400},
		{`PMTA API error (HTTP 429): too many`, 429},
		{`pmta api error (http 500): lowercase`, 500},
		{`no bridge prefix: 502`, -1},
		{`PMTA API request to http://...: i/o timeout`, -1},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, pmtaHTTPStatus(tc.in), "input: %s", tc.in)
	}
}

func TestIsTransportError_FalseForISPRejections(t *testing.T) {
	cases := []struct {
		name   string
		errMsg string
	}{
		{"550 user unknown", `550 5.1.1 The email account that you tried to reach does not exist`},
		{"550 blocked", `550 5.7.1 [15.204.22.178] Gmail has detected that this message is likely suspicious`},
		{"551 relay denied", `551 relay denied`},
		{"552 over quota", `552 5.2.2 mailbox full`},
		{"553 invalid address", `553 5.1.3 Invalid address`},
		{"554 transaction failed", `554 5.7.1 message rejected`},
		{"user unknown", `user unknown`},
		{"mailbox not found", `mailbox not found for recipient`},
		{"does not exist", `The email address does not exist`},
		{"no such user", `no such user here`},
		{"invalid recipient", `invalid recipient address`},
		{"sender rejected", `550 5.1.0 <hello@em.quizfiesta.com> sender rejected`},
		{"permanently disabled", `account permanently disabled`},
		{"deactivated", `mailbox deactivated`},
		{"unknown error", `unknown error`},
		{"PMTA HTTP 400", `PMTA API error (HTTP 400): {"error":"invalid envelope"}`},
		{"PMTA HTTP 422", `PMTA API error (HTTP 422): {"error":"missing recipient"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.False(t, isTransportError(tc.errMsg),
				"should NOT be transport error for: %s", tc.errMsg)
		})
	}
}

func TestClassifySendError_HardBounce(t *testing.T) {
	cases := []struct {
		name   string
		errMsg string
		want   string
	}{
		{"550 user unknown", "550 5.1.1 user unknown", "hard"},
		{"551 relay", "551 relay denied", "hard"},
		{"552 quota", "552 mailbox full", "hard"},
		{"553 address", "553 invalid address format", "hard"},
		{"554 rejected", "554 message rejected", "hard"},
		{"mailbox not found", "mailbox not found", "hard"},
		{"does not exist", "The email account does not exist", "hard"},
		{"no such user", "no such user here", "hard"},
		{"invalid recipient", "invalid recipient", "hard"},
		{"rejected text", "Address rejected", "hard"},
		{"permanently", "permanently unavailable", "hard"},
		{"disabled", "account disabled", "hard"},
		{"deactivated", "mailbox deactivated", "hard"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, classifySendError(tc.errMsg))
		})
	}
}

func TestClassifySendError_SoftBounce(t *testing.T) {
	cases := []struct {
		name   string
		errMsg string
	}{
		{"temporary failure", "421 temporary failure, try again later"},
		{"rate limited", "421 4.7.0 rate limited"},
		{"try again", "please try again later"},
		{"service unavailable", "service temporarily unavailable"},
		{"generic error", "unknown error"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, "soft", classifySendError(tc.errMsg))
		})
	}
}

func TestTransportAndBounceClassification_Orthogonal(t *testing.T) {
	t.Run("transport errors should never reach classifySendError in production", func(t *testing.T) {
		transportErr := "PMTA API request to http://15.204.101.125:19099: dial tcp: i/o timeout"
		assert.True(t, isTransportError(transportErr),
			"should be detected as transport error before classifySendError is called")
	})

	t.Run("ISP rejections are not transport errors", func(t *testing.T) {
		ispErr := "550 5.7.1 Gmail has detected that this message is likely suspicious"
		assert.False(t, isTransportError(ispErr))
		assert.Equal(t, "hard", classifySendError(ispErr))
	})
}
