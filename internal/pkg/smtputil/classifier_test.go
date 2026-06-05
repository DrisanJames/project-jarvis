package smtputil

import (
	"errors"
	"testing"
)

func TestClassifyError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want BounceType
	}{
		{"nil error", nil, BounceSoft},
		{"550 user unknown", errors.New("550 5.1.1 User unknown"), BounceHard},
		// Reputation/system blocks must NOT be hard (would wrongly suppress a
		// valid recipient). Charter/Spectrum 5.3.2 is the canonical case.
		{"5.3.2 system not accepting", errors.New("550 5.3.2 system not accepting network messages"), BounceSoft},
		{"5.7.1 policy block", errors.New("550 5.7.1 message blocked due to policy"), BounceSoft},
		{"5.4.x routing block", errors.New("554 5.4.4 unable to route"), BounceSoft},
		{"551 relay denied", errors.New("551 relay not permitted"), BounceHard},
		{"552 mailbox full hard", errors.New("552 mailbox full"), BounceHard},
		{"553 invalid address", errors.New("553 invalid mailbox"), BounceHard},
		{"554 transaction failed", errors.New("554 transaction failed"), BounceHard},
		{"421 service unavailable", errors.New("421 service not available"), BounceSoft},
		{"450 mailbox busy", errors.New("450 mailbox temporarily unavailable"), BounceSoft},
		{"451 local error", errors.New("451 requested action aborted"), BounceSoft},
		{"452 insufficient storage", errors.New("452 insufficient system storage"), BounceSoft},
		{"connection refused", errors.New("dial tcp: connection refused"), BounceSoft},
		{"timeout", errors.New("i/o timeout"), BounceSoft},
		{"TLS handshake", errors.New("tls: handshake failure"), BounceSoft},
		{"generic error", errors.New("something went wrong"), BounceSoft},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyError(tt.err)
			if got != tt.want {
				t.Errorf("ClassifyError(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

func TestClassifyBounce(t *testing.T) {
	tests := []struct {
		name              string
		dsn, cat, diag    string
		want              BounceClass
	}{
		{"5.1.1 bad mailbox", "5.1.1", "bad-mailbox", "", ClassHard},
		{"5.1.2 bad domain", "5.1.2", "bad-domain", "", ClassHard},
		{"5.2.1 disabled", "5.2.1", "inactive-mailbox", "", ClassHard},
		{"5.2.2 mailbox full -> soft", "5.2.2", "quota-issues", "", ClassSoft},
		// DSN overrides PMTA's mislabeled bad-mailbox for Charter/Spectrum.
		{"5.3.2 mislabeled bad-mailbox", "5.3.2", "bad-mailbox", "system not accepting network messages", ClassBlock},
		{"5.7.1 policy", "5.7.1", "policy-related", "blocked", ClassBlock},
		{"5.4.4 routing", "5.4.4", "routing-errors", "", ClassBlock},
		{"4.2.2 transient", "4.2.2", "quota-issues", "", ClassSoft},
		{"no DSN, policy cat", "", "policy-related", "", ClassBlock},
		{"no DSN, hard cat", "", "bad-mailbox", "", ClassHard},
		{"no DSN, connection cat -> soft", "", "no-answer-from-host", "", ClassSoft},
		{"DSN from diag fallback", "", "bad-mailbox", "5.3.2 (system not accepting network messages)", ClassBlock},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyBounce(tt.dsn, tt.cat, tt.diag); got != tt.want {
				t.Errorf("ClassifyBounce(%q,%q,%q) = %d, want %d", tt.dsn, tt.cat, tt.diag, got, tt.want)
			}
		})
	}
}

func TestIsReputationOrSystemBlock(t *testing.T) {
	block := []string{"5.3.2", "5.3.0", "5.4.4", "5.5.0", "5.7.1"}
	notBlock := []string{"5.1.1", "5.2.1", "5.2.2", "4.3.2", "", "2.0.0"}
	for _, d := range block {
		if !IsReputationOrSystemBlock(d) {
			t.Errorf("IsReputationOrSystemBlock(%q) = false, want true", d)
		}
	}
	for _, d := range notBlock {
		if IsReputationOrSystemBlock(d) {
			t.Errorf("IsReputationOrSystemBlock(%q) = true, want false", d)
		}
	}
}
