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
