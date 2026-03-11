package worker

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestVMTAShortName(t *testing.T) {
	tests := []struct {
		name     string
		hostname string
		want     string
	}{
		{"full FQDN", "mta1.mail.projectjarvis.io", "mta1"},
		{"two-part hostname", "mta2.example.com", "mta2"},
		{"already short", "mta3", "mta3"},
		{"empty string", "", ""},
		{"single char prefix", "m.example.com", "m"},
		{"ip-like hostname", "15.204.101.125", "15"},
		{"dash in prefix", "mta-1.mail.example.com", "mta-1"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := vmtaShortName(tc.hostname)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestVMTAShortName_ValidationRules(t *testing.T) {
	// VMTA names that would cause PMTA misconfiguration
	badHostnames := []struct {
		hostname string
		reason   string
	}{
		{"", "empty hostname produces empty VMTA"},
		{"a.b.c", "single-char VMTA 'a' may not match any directive"},
	}

	for _, tc := range badHostnames {
		vmta := vmtaShortName(tc.hostname)
		if vmta != "" && len(vmta) < 2 {
			t.Logf("WARNING: hostname %q produces short VMTA %q — %s", tc.hostname, vmta, tc.reason)
		}
	}
}
