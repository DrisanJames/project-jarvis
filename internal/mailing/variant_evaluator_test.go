package mailing

import (
	"testing"
)

func TestStatisticalSignificance(t *testing.T) {
	ve := &VariantEvaluator{
		pThreshold: 0.05,
	}

	tests := []struct {
		name          string
		rateA, rateB  float64
		nA, nB        int
		wantSignificant bool
	}{
		{"clearly different", 0.30, 0.10, 1000, 1000, true},
		{"identical rates", 0.20, 0.20, 1000, 1000, false},
		{"small difference small sample", 0.22, 0.20, 50, 50, false},
		{"small difference large sample", 0.22, 0.18, 5000, 5000, true},
		{"zero sends", 0.20, 0.10, 0, 1000, false},
		{"both zero", 0.0, 0.0, 1000, 1000, false},
		{"both 100%", 1.0, 1.0, 1000, 1000, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ve.isStatisticallySignificant(tt.rateA, tt.rateB, tt.nA, tt.nB)
			if got != tt.wantSignificant {
				t.Errorf("isStatisticallySignificant(%.2f, %.2f, %d, %d) = %v, want %v",
					tt.rateA, tt.rateB, tt.nA, tt.nB, got, tt.wantSignificant)
			}
		})
	}
}
