package api

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/ignite/sparkpost-monitor/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestPreflightDeployCheck_NoProfile(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectQuery("SELECT id::text, ip_pool").
		WithArgs("org-1", "em.example.com").
		WillReturnRows(sqlmock.NewRows([]string{"id", "ip_pool"}))

	res := preflightDeployCheck(context.Background(), db, "org-1", "em.example.com")
	assert.False(t, res.OK)
	require.Len(t, res.Errors, 1)
	assert.Equal(t, "sending_profile", res.Errors[0].Check)
}

func TestPreflightDeployCheck_EmptyIPPool(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectQuery("SELECT id::text, ip_pool").
		WithArgs("org-1", "em.example.com").
		WillReturnRows(sqlmock.NewRows([]string{"id", "ip_pool"}).AddRow("profile-1", ""))

	res := preflightDeployCheck(context.Background(), db, "org-1", "em.example.com")
	assert.False(t, res.OK)
	require.Len(t, res.Errors, 1)
	assert.Equal(t, "ip_pool", res.Errors[0].Check)
}

func TestPreflightDeployCheck_NoActiveIPs(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectQuery("SELECT id::text, ip_pool").
		WithArgs("org-1", "em.example.com").
		WillReturnRows(sqlmock.NewRows([]string{"id", "ip_pool"}).AddRow("profile-1", "warmup-pool"))

	mock.ExpectQuery("SELECT ip.hostname, ip.status").
		WithArgs("warmup-pool").
		WillReturnRows(sqlmock.NewRows([]string{"hostname", "status"}))

	res := preflightDeployCheck(context.Background(), db, "org-1", "em.example.com")
	assert.False(t, res.OK)
	require.Len(t, res.Errors, 1)
	assert.Equal(t, "ip_pool_empty", res.Errors[0].Check)
}

func TestWaveSanityCheck_TooFewWaves(t *testing.T) {
	plans := []pmtaNormalizedPlan{{ISP: "gmail"}}
	start := time.Now()
	wavesByISP := map[string][]pmtaWaveSpec{
		"gmail": {
			{WaveNumber: 1, ScheduledAt: start},
			{WaveNumber: 2, ScheduledAt: start.Add(15 * time.Minute)},
		},
	}
	err := waveSanityCheck(plans, wavesByISP)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "only 2 waves")
}

func TestWaveSanityCheck_Valid(t *testing.T) {
	plans := []pmtaNormalizedPlan{{ISP: "gmail"}}
	start := time.Now()
	wavesByISP := map[string][]pmtaWaveSpec{
		"gmail": {
			{WaveNumber: 1, ScheduledAt: start},
			{WaveNumber: 2, ScheduledAt: start.Add(1 * time.Hour)},
			{WaveNumber: 3, ScheduledAt: start.Add(2 * time.Hour)},
			{WaveNumber: 4, ScheduledAt: start.Add(3 * time.Hour)},
			{WaveNumber: 5, ScheduledAt: start.Add(4 * time.Hour)},
		},
	}
	err := waveSanityCheck(plans, wavesByISP)
	assert.NoError(t, err)
}
