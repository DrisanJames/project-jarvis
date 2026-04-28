package worker

import "testing"

func TestIsPMTATransient(t *testing.T) {
	tests := []struct {
		name string
		msg  string
		want bool
	}{
		// PMTA-local infra failures (must NOT burn recipient retry budget).
		{"connection refused (pmtad down)", `PMTA API error (HTTP 502): {"status": "error", "detail": "[Errno 111] Connection refused"}`, true},
		{"smtplib timeout (resolver stalled)", `PMTA API error (HTTP 502): {"status": "error", "detail": "Connection unexpectedly closed: timed out"}`, true},
		{"bridge wrapped local 421", `PMTA API error (HTTP 502): {"status": "error", "detail": "(421, b'mta-db-gn7.mail.em.discountblog.com')"}`, true},
		{"connection reset by peer", `PMTA API request to http://15.204.101.125:19099/api/inject/v1: dial tcp: connection reset by peer`, true},
		{"broken pipe", `write to bridge: broken pipe`, true},
		{"raw EOF from bridge", `PMTA API request: read tcp: EOF`, true},
		{"http I/O timeout", `Post "http://15.204.101.125:19099/api/inject/v1": net/http: request canceled (i/o timeout)`, true},
		{"context deadline exceeded", `PMTA API request: context deadline exceeded`, true},
		{"go stdlib client timeout", `net/http: request canceled (Client.Timeout exceeded while awaiting headers)`, true},
		{"idle connection closed", `PMTA API request: server closed idle connection`, true},
		{"DNS resolution to bridge host", `PMTA API request: dial tcp: lookup pmta-bridge.internal: no such host`, true},
		{"DNS temporary failure", `PMTA API request: temporary failure in name resolution`, true},
		{"explicit PMTA API error prefix", `PMTA API error (HTTP 503): backend unavailable`, true},
		{"HTTP 500 from bridge", `PMTA API error (HTTP 500): bridge crashed`, true},
		{"HTTP 504 gateway timeout", `PMTA API error (HTTP 504): upstream timeout`, true},
		{"service unavailable", `service unavailable`, true},

		// Recipient-attributable failures (MUST keep existing dead-letter behavior).
		// Note: in this codebase the bridge only emits "PMTA API error" on HTTP>=400
		// (bridge / pmtad-side failures). Recipient SMTP errors surface separately
		// from the PMTA accounting pipeline as bare SMTP strings without that prefix.
		{"550 mailbox does not exist (accounting path)", `SMTP 550 5.1.1 mailbox does not exist`, false},
		{"552 message size exceeded", `552 5.3.4 message size exceeded`, false},
		{"554 policy rejection", `554 5.7.1 policy rejection by destination ISP`, false},
		{"DKIM verification failed at recipient", `DKIM verification failed: signature broken`, false},
		{"SPF rejection at recipient", `SPF check failed: sender not authorized`, false},
		{"recipient blacklisted", `recipient mailbox temporarily blacklisted`, false},
		{"complaint feedback loop", `recipient submitted complaint`, false},

		// Empty / unknown — default to "not transient" so existing behavior wins.
		{"empty string", ``, false},
		{"plain string", `something happened`, false},

		// Case insensitivity.
		{"uppercase Connection Refused", `Error: Connection Refused while reaching bridge`, true},
		{"mixed case PMTA API Error", `PMTA Api Error (HTTP 502)`, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsPMTATransient(tt.msg)
			if got != tt.want {
				t.Errorf("IsPMTATransient(%q) = %v, want %v", tt.msg, got, tt.want)
			}
		})
	}
}

func TestPMTATransientBackoffMinutes(t *testing.T) {
	if PMTATransientBackoffMinutes != 10 {
		t.Errorf("PMTATransientBackoffMinutes = %d, want 10", PMTATransientBackoffMinutes)
	}
}
