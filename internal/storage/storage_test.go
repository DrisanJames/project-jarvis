package storage

import (
	"context"
	"testing"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/config"
	"github.com/ignite/sparkpost-monitor/internal/sparkpost"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestStorage(t *testing.T) *Storage {
	tmpDir := t.TempDir()
	cfg := config.StorageConfig{
		Type:      "local",
		LocalPath: tmpDir,
	}
	
	s, err := New(cfg)
	require.NoError(t, err)
	return s
}

func TestNew(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := config.StorageConfig{
		Type:      "local",
		LocalPath: tmpDir,
	}

	s, err := New(cfg)
	require.NoError(t, err)
	require.NotNil(t, s)

	assert.NotNil(t, s.metricsCache)
	assert.NotNil(t, s.baselines)
}

func TestSaveAndGetMetrics(t *testing.T) {
	s := newTestStorage(t)
	ctx := context.Background()

	metrics := []sparkpost.ProcessedMetrics{
		{
			Timestamp:    time.Now(),
			Source:       "sparkpost",
			GroupBy:      "summary",
			GroupValue:   "all",
			Targeted:     1000000,
			Delivered:    980000,
			DeliveryRate: 0.98,
		},
	}

	err := s.SaveMetrics(ctx, metrics)
	require.NoError(t, err)

	// Verify it's in cache
	from := time.Now().Add(-1 * time.Hour)
	to := time.Now().Add(1 * time.Hour)
	
	retrieved, err := s.GetHistoricalMetrics(ctx, from, to)
	require.NoError(t, err)
	require.Len(t, retrieved, 1)
	assert.Equal(t, int64(1000000), retrieved[0].Targeted)
}

func TestSaveISPMetrics(t *testing.T) {
	s := newTestStorage(t)
	ctx := context.Background()

	metrics := []sparkpost.ISPMetrics{
		{
			Provider: "Gmail",
			Metrics: sparkpost.ProcessedMetrics{
				Targeted:  500000,
				Delivered: 495000,
			},
			Status: "healthy",
		},
		{
			Provider: "Yahoo",
			Metrics: sparkpost.ProcessedMetrics{
				Targeted:  300000,
				Delivered: 290000,
			},
			Status: "warning",
		},
	}

	err := s.SaveISPMetrics(ctx, metrics)
	require.NoError(t, err)

	// Verify cache
	from := time.Now().Add(-1 * time.Hour)
	to := time.Now().Add(1 * time.Hour)
	
	retrieved, err := s.GetHistoricalISPMetrics(ctx, from, to)
	require.NoError(t, err)
	require.Len(t, retrieved, 2)
}

func TestSaveSignals(t *testing.T) {
	s := newTestStorage(t)
	ctx := context.Background()

	signals := sparkpost.SignalsData{
		Timestamp: time.Now(),
		BounceReasons: []sparkpost.BounceReasonResult{
			{
				Reason:      "550 User unknown",
				CountBounce: 1000,
			},
		},
	}

	err := s.SaveSignals(ctx, signals)
	require.NoError(t, err)

	retrieved, err := s.GetRecentSignals(ctx, 10)
	require.NoError(t, err)
	require.Len(t, retrieved, 1)
	assert.Len(t, retrieved[0].BounceReasons, 1)
}

func TestSaveAndGetBaseline(t *testing.T) {
	s := newTestStorage(t)
	ctx := context.Background()

	baseline := &Baseline{
		EntityType: "isp",
		EntityName: "Gmail",
		Metrics: map[string]*MetricBaseline{
			"complaint_rate": {
				Mean:   0.00012,
				StdDev: 0.00004,
			},
		},
		UpdatedAt:  time.Now(),
		DataPoints: 100,
	}

	err := s.SaveBaseline(ctx, baseline)
	require.NoError(t, err)

	retrieved, err := s.GetBaseline(ctx, "isp", "Gmail")
	require.NoError(t, err)
	require.NotNil(t, retrieved)

	assert.Equal(t, "Gmail", retrieved.EntityName)
	assert.Equal(t, 0.00012, retrieved.Metrics["complaint_rate"].Mean)
}

func TestGetNonExistentBaseline(t *testing.T) {
	s := newTestStorage(t)
	ctx := context.Background()

	baseline, err := s.GetBaseline(ctx, "isp", "NonExistent")
	require.NoError(t, err)
	assert.Nil(t, baseline)
}

func TestSaveCorrelations(t *testing.T) {
	s := newTestStorage(t)
	ctx := context.Background()

	correlations := []Correlation{
		{
			EntityType:       "isp",
			EntityName:       "Yahoo",
			TriggerMetric:    "volume",
			TriggerThreshold: 3000000,
			TriggerOperator:  "gt",
			EffectMetric:     "complaint_rate",
			EffectChange:     0.45,
			Confidence:       0.82,
			Occurrences:      15,
		},
	}

	err := s.SaveCorrelations(ctx, correlations)
	require.NoError(t, err)

	retrieved, err := s.GetCorrelations(ctx)
	require.NoError(t, err)
	require.Len(t, retrieved, 1)
	assert.Equal(t, "Yahoo", retrieved[0].EntityName)
	assert.Equal(t, 0.82, retrieved[0].Confidence)
}

func TestSignalsCache_MaxSize(t *testing.T) {
	s := newTestStorage(t)
	ctx := context.Background()

	// Add more than 1000 signals
	for i := 0; i < 1100; i++ {
		signals := sparkpost.SignalsData{
			Timestamp: time.Now(),
		}
		s.SaveSignals(ctx, signals)
	}

	// Should be capped at 1000
	retrieved, err := s.GetRecentSignals(ctx, 2000)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(retrieved), 1000)
}

func TestGetCacheStats(t *testing.T) {
	s := newTestStorage(t)
	ctx := context.Background()

	// Add some data
	s.SaveMetrics(ctx, []sparkpost.ProcessedMetrics{{Targeted: 100}})
	s.SaveISPMetrics(ctx, []sparkpost.ISPMetrics{{Provider: "Gmail"}})
	s.SaveBaseline(ctx, &Baseline{EntityType: "isp", EntityName: "Gmail"})

	stats := s.GetCacheStats()
	
	assert.Equal(t, 1, stats["metrics_days"])
	assert.Equal(t, 1, stats["isp_days"])
	assert.Equal(t, 1, stats["baselines_count"])
}

func TestClearCache(t *testing.T) {
	s := newTestStorage(t)
	ctx := context.Background()

	// Add data
	s.SaveMetrics(ctx, []sparkpost.ProcessedMetrics{{Targeted: 100}})
	s.SaveSignals(ctx, sparkpost.SignalsData{})

	// Clear
	s.ClearCache()

	// Verify empty
	stats := s.GetCacheStats()
	assert.Equal(t, 0, stats["metrics_days"])
	assert.Equal(t, 0, stats["signals_count"])
}

func TestGetAllBaselines(t *testing.T) {
	s := newTestStorage(t)
	ctx := context.Background()

	// Add multiple baselines
	s.SaveBaseline(ctx, &Baseline{EntityType: "isp", EntityName: "Gmail"})
	s.SaveBaseline(ctx, &Baseline{EntityType: "isp", EntityName: "Yahoo"})
	s.SaveBaseline(ctx, &Baseline{EntityType: "ip", EntityName: "18.236.253.72"})

	baselines, err := s.GetAllBaselines(ctx)
	require.NoError(t, err)
	assert.Len(t, baselines, 3)
}

func TestConcurrentAccess(t *testing.T) {
	s := newTestStorage(t)
	ctx := context.Background()

	done := make(chan bool)

	// Concurrent writes
	go func() {
		for i := 0; i < 100; i++ {
			s.SaveMetrics(ctx, []sparkpost.ProcessedMetrics{{Targeted: int64(i)}})
		}
		done <- true
	}()

	// Concurrent reads
	go func() {
		for i := 0; i < 100; i++ {
			s.GetCacheStats()
		}
		done <- true
	}()

	<-done
	<-done
}
