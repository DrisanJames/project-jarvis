package worker

import "testing"

func TestNewPartnerDripOrchestratorThrottleDeferralDisabled(t *testing.T) {
	orch := NewPartnerDripOrchestrator(nil, PartnerDripOrchestratorConfig{
		ThrottleDeferralDisabled:  true,
		ThrottledISPRateThreshold: 0,
	})
	if !orch.cfg.ThrottleDeferralDisabled {
		t.Fatal("expected ThrottleDeferralDisabled=true")
	}
	if orch.cfg.ThrottledISPRateThreshold != 0 {
		t.Fatalf("threshold should stay 0 when deferral disabled, got %v", orch.cfg.ThrottledISPRateThreshold)
	}
}

func TestNewPartnerDripOrchestratorThrottleThresholdDefault(t *testing.T) {
	orch := NewPartnerDripOrchestrator(nil, PartnerDripOrchestratorConfig{
		ThrottledISPRateThreshold: 0,
	})
	if orch.cfg.ThrottledISPRateThreshold != 50 {
		t.Fatalf("unset zero threshold should default to 50, got %v", orch.cfg.ThrottledISPRateThreshold)
	}
}
