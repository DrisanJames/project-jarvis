package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoad(t *testing.T) {
	// Create a temporary config file
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	
	configContent := `
server:
  port: 9090
  host: "0.0.0.0"

sparkpost:
  api_key: "test-api-key"
  base_url: "https://api.sparkpost.com/api/v1"
  timeout_seconds: 45

polling:
  interval_seconds: 120
  historical_days: 60
  analysis_window_days: 14

storage:
  type: "local"
  local_path: "./test-data"

agent:
  baseline_recalc_hours: 12
  correlation_recalc_hours: 72
  anomaly_sigma: 2.5
  min_data_points: 50

fallback_thresholds:
  complaint_rate_warning: 0.0003
  complaint_rate_critical: 0.0005
  bounce_rate_warning: 0.03
  bounce_rate_critical: 0.05
  block_rate_warning: 0.003
  block_rate_critical: 0.005
`
	err := os.WriteFile(configPath, []byte(configContent), 0644)
	require.NoError(t, err)

	cfg, err := Load(configPath)
	require.NoError(t, err)

	// Test server config
	assert.Equal(t, 9090, cfg.Server.Port)
	assert.Equal(t, "0.0.0.0", cfg.Server.Host)

	// Test SparkPost config
	assert.Equal(t, "test-api-key", cfg.SparkPost.APIKey)
	assert.Equal(t, "https://api.sparkpost.com/api/v1", cfg.SparkPost.BaseURL)
	assert.Equal(t, 45, cfg.SparkPost.TimeoutSeconds)

	// Test polling config
	assert.Equal(t, 120, cfg.Polling.IntervalSeconds)
	assert.Equal(t, 60, cfg.Polling.HistoricalDays)
	assert.Equal(t, 14, cfg.Polling.AnalysisWindowDays)

	// Test storage config
	assert.Equal(t, "local", cfg.Storage.Type)
	assert.Equal(t, "./test-data", cfg.Storage.LocalPath)

	// Test agent config
	assert.Equal(t, 12, cfg.Agent.BaselineRecalcHours)
	assert.Equal(t, 72, cfg.Agent.CorrelationRecalcHours)
	assert.Equal(t, 2.5, cfg.Agent.AnomalySigma)
	assert.Equal(t, 50, cfg.Agent.MinDataPoints)

	// Test threshold config
	assert.Equal(t, 0.0003, cfg.FallbackThresholds.ComplaintRateWarning)
	assert.Equal(t, 0.0005, cfg.FallbackThresholds.ComplaintRateCritical)
}

func TestLoadDefaults(t *testing.T) {
	// Create a minimal config file
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	
	configContent := `
sparkpost:
  api_key: "test-key"
`
	err := os.WriteFile(configPath, []byte(configContent), 0644)
	require.NoError(t, err)

	cfg, err := Load(configPath)
	require.NoError(t, err)

	// Verify defaults are applied
	assert.Equal(t, 8080, cfg.Server.Port)
	assert.Equal(t, "localhost", cfg.Server.Host)
	assert.Equal(t, 30, cfg.SparkPost.TimeoutSeconds)
	assert.Equal(t, 60, cfg.Polling.IntervalSeconds)
	assert.Equal(t, 30, cfg.Polling.HistoricalDays)
	assert.Equal(t, 7, cfg.Polling.AnalysisWindowDays)
	assert.Equal(t, 2.0, cfg.Agent.AnomalySigma)
	assert.Equal(t, 100, cfg.Agent.MinDataPoints)
}

func TestLoadFromEnv(t *testing.T) {
	// Create a minimal config file
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	
	configContent := `
sparkpost:
  api_key: "file-key"
  base_url: "https://file-url.com"
`
	err := os.WriteFile(configPath, []byte(configContent), 0644)
	require.NoError(t, err)

	// Set environment variables
	os.Setenv("SPARKPOST_API_KEY", "env-key")
	os.Setenv("SPARKPOST_BASE_URL", "https://env-url.com")
	defer func() {
		os.Unsetenv("SPARKPOST_API_KEY")
		os.Unsetenv("SPARKPOST_BASE_URL")
	}()

	cfg, err := LoadFromEnv(configPath)
	require.NoError(t, err)

	// Environment variables should override file values
	assert.Equal(t, "env-key", cfg.SparkPost.APIKey)
	assert.Equal(t, "https://env-url.com", cfg.SparkPost.BaseURL)
}

func TestLoadFileNotFound(t *testing.T) {
	_, err := Load("/nonexistent/path/config.yaml")
	assert.Error(t, err)
}

func TestTimeout(t *testing.T) {
	cfg := SparkPostConfig{TimeoutSeconds: 45}
	assert.Equal(t, 45*1000000000, int(cfg.Timeout().Nanoseconds()))
}

func TestInterval(t *testing.T) {
	cfg := PollingConfig{IntervalSeconds: 120}
	assert.Equal(t, 120*1000000000, int(cfg.Interval().Nanoseconds()))
}
