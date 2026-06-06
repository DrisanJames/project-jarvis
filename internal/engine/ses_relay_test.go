package engine

import "testing"

func TestIsPMTARelayedToSES(t *testing.T) {
	cases := []struct {
		name string
		pool string
		vmta string
		want bool
	}{
		{"ses tenant pool", "db-ses-pool", "vmta-db-ses", true},
		{"ses pool only", "qf-ses-pool", "", true},
		{"ses vmta only", "", "vmta-ht-ses", true},
		{"legacy ses relay", "ses-relay-a", "ses-relay", true},
		{"uppercase SES", "DB-SES-POOL", "", true},
		{"direct gmail pool", "db-gmail-pool", "vmta-db-gm1", false},
		{"general pool", "db-general-pool", "vmta-db-gen", false},
		{"empty", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := IsPMTARelayedToSES(AccountingRecord{Pool: c.pool, VMTA: c.vmta})
			if got != c.want {
				t.Fatalf("IsPMTARelayedToSES(pool=%q vmta=%q) = %v, want %v", c.pool, c.vmta, got, c.want)
			}
		})
	}
}
