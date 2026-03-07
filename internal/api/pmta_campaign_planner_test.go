package api

import (
	"testing"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/engine"
)

func TestNormalizePMTACampaignInput_LegacyScheduledTranslation(t *testing.T) {
	scheduledAt := time.Now().UTC().Add(15 * time.Minute).Round(time.Minute)
	input := engine.PMTACampaignInput{
		Name:          "Legacy PMTA",
		TargetISPs:    []engine.ISP{engine.ISPGmail, engine.ISPApple},
		SendingDomain: "mail.example.com",
		Variants: []engine.ContentVariant{{
			VariantName: "A",
			Subject:     "Subject",
			HTMLContent: "<html></html>",
		}},
		Timezone:         "UTC",
		ThrottleStrategy: "auto",
		SendMode:         "scheduled",
		ScheduledAt:      &scheduledAt,
		ISPQuotas: []engine.ISPQuota{
			{ISP: "gmail", Volume: 100},
		},
	}

	normalized, err := normalizePMTACampaignInput(input)
	if err != nil {
		t.Fatalf("normalizePMTACampaignInput() error = %v", err)
	}
	if !normalized.LegacyInput {
		t.Fatalf("expected legacy input translation")
	}
	if len(normalized.Plans) != 2 {
		t.Fatalf("expected 2 ISP plans, got %d", len(normalized.Plans))
	}
	if normalized.Plans[0].TimeSpans[0].StartAt.IsZero() {
		t.Fatalf("expected translated time span")
	}

	quotas := map[string]int{}
	for _, plan := range normalized.Plans {
		quotas[plan.ISP] = plan.Quota
		if plan.TimeSpans[0].StartAt.UTC() != scheduledAt.UTC() {
			t.Fatalf("expected start time %s, got %s", scheduledAt.UTC(), plan.TimeSpans[0].StartAt.UTC())
		}
	}
	if quotas["gmail"] != 100 {
		t.Fatalf("expected gmail quota 100, got %d", quotas["gmail"])
	}
	if quotas["apple"] != 0 {
		t.Fatalf("expected apple quota 0, got %d", quotas["apple"])
	}
}

func TestBuildPMTAWaveSpecs_IntervalCadence(t *testing.T) {
	start := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	plan := pmtaNormalizedPlan{
		ISP: "gmail",
		Cadence: engine.PMTACadenceInput{
			Mode:         "interval",
			EveryMinutes: 15,
			BatchSize:    100,
		},
		TimeSpans: []pmtaNormalizedTimeSpan{{
			StartAt: start,
			EndAt:   start.Add(45 * time.Minute),
		}},
	}

	waves := buildPMTAWaveSpecs("campaign-1", plan, 250)
	if len(waves) != 3 {
		t.Fatalf("expected 3 waves, got %d", len(waves))
	}
	if waves[0].PlannedRecipients != 100 || waves[1].PlannedRecipients != 100 || waves[2].PlannedRecipients != 50 {
		t.Fatalf("unexpected wave distribution: %+v", waves)
	}
	if !waves[1].ScheduledAt.Equal(start.Add(15 * time.Minute)) {
		t.Fatalf("expected second wave at %s, got %s", start.Add(15*time.Minute), waves[1].ScheduledAt)
	}
}
