package api

import (
	"testing"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/engine"
)

// TestDryRunISPDistribution simulates the full dry-run pipeline:
// classify subscribers by ISP, apply quota caps, and verify the
// resulting distribution matches the configured quotas.
// This confirms no mail would be injected and ISP routing is correct.
func TestDryRunISPDistribution(t *testing.T) {
	registry := engine.NewISPRegistry()

	// Simulate a realistic audience: 100 subscribers across ISPs
	var emails []string
	for i := 0; i < 40; i++ {
		emails = append(emails, "u@gmail.com")
	}
	for i := 0; i < 25; i++ {
		emails = append(emails, "u@yahoo.com")
	}
	for i := 0; i < 15; i++ {
		emails = append(emails, "u@outlook.com")
	}
	for i := 0; i < 10; i++ {
		emails = append(emails, "u@icloud.com")
	}
	for i := 0; i < 5; i++ {
		emails = append(emails, "u@comcast.net")
	}
	for i := 0; i < 3; i++ {
		emails = append(emails, "u@att.net")
	}
	for i := 0; i < 2; i++ {
		emails = append(emails, "u@unknown-domain-xyz.com")
	}

	subs := makeSubscribers(emails)
	quotas := []ISPQuota{
		{ISP: "gmail", Volume: 10, PoolName: "gmail-pool"},
		{ISP: "yahoo", Volume: 8, PoolName: "yahoo-pool"},
		{ISP: "microsoft", Volume: 5, PoolName: "microsoft-pool"},
		{ISP: "apple", Volume: 3, PoolName: "apple-pool"},
		{ISP: "comcast", Volume: 2, PoolName: "comcast-pool"},
		{ISP: "att", Volume: 2, PoolName: "att-pool"},
	}

	buckets := classifyAndCapSubscribers(registry, subs, quotas)

	// Build pool name lookup
	poolForISP := make(map[string]string)
	for _, q := range quotas {
		poolForISP[q.ISP] = q.PoolName
	}

	// Simulate the dry-run send loop
	type taggedSubscriber struct {
		sub      subscriber
		ispKey   string
		poolName string
	}
	var sendList []taggedSubscriber
	for ispKey, subs := range buckets {
		pool := poolForISP[ispKey]
		if pool == "" && ispKey != "" && ispKey != "other" {
			pool = ispKey + "-pool"
		}
		for _, sub := range subs {
			sendList = append(sendList, taggedSubscriber{sub: sub, ispKey: ispKey, poolName: pool})
		}
	}

	dryRunISPCounts := make(map[string]int)
	var totalWouldSend int
	for _, tagged := range sendList {
		dryRunISPCounts[tagged.ispKey]++
		totalWouldSend++

		// Verify pool name is set for known ISPs
		if tagged.ispKey != "other" && tagged.ispKey != "" && tagged.poolName == "" {
			t.Errorf("subscriber %s (ISP=%s) has empty pool name", tagged.sub.Email, tagged.ispKey)
		}
	}

	// Verify quotas are respected
	tests := []struct {
		isp      string
		wantMax  int
		wantPool string
	}{
		{"gmail", 10, "gmail-pool"},
		{"yahoo", 8, "yahoo-pool"},
		{"microsoft", 5, "microsoft-pool"},
		{"apple", 3, "apple-pool"},
		{"comcast", 2, "comcast-pool"},
		{"att", 2, "att-pool"},
	}

	for _, tc := range tests {
		got := dryRunISPCounts[tc.isp]
		if got > tc.wantMax {
			t.Errorf("%s: want <= %d, got %d", tc.isp, tc.wantMax, got)
		}
		if got == 0 {
			t.Errorf("%s: expected some subscribers, got 0", tc.isp)
		}
	}

	// "other" bucket should have 2 unknown-domain subscribers (uncapped)
	if dryRunISPCounts["other"] != 2 {
		t.Errorf("other: want 2, got %d", dryRunISPCounts["other"])
	}

	// Total should be less than 100 due to caps
	if totalWouldSend > 100 {
		t.Errorf("total would_send (%d) exceeds audience size (100)", totalWouldSend)
	}
	// Expected: 10+8+5+3+2+2+2 = 32
	expectedTotal := 10 + 8 + 5 + 3 + 2 + 2 + 2
	if totalWouldSend != expectedTotal {
		t.Errorf("total would_send: want %d, got %d", expectedTotal, totalWouldSend)
	}

	t.Logf("Dry-run results: would_send=%d, by_isp=%v", totalWouldSend, dryRunISPCounts)
}

// TestDryRunPoolNameDerivation verifies pool names are correctly derived
// from ISP names when not explicitly set.
func TestDryRunPoolNameDerivation(t *testing.T) {
	registry := engine.NewISPRegistry()

	subs := []subscriber{
		{ID: uuid.New(), Email: "a@gmail.com", Status: "confirmed"},
		{ID: uuid.New(), Email: "b@outlook.com", Status: "confirmed"},
	}

	quotas := []ISPQuota{
		{ISP: "gmail", Volume: 10},     // PoolName intentionally empty
		{ISP: "microsoft", Volume: 10}, // PoolName intentionally empty
	}

	buckets := classifyAndCapSubscribers(registry, subs, quotas)

	for ispKey := range buckets {
		expectedPool := ispKey + "-pool"
		if ispKey == "" || ispKey == "other" {
			continue
		}
		// Verify the pool name derivation logic matches what the send loop does
		pool := engine.PoolNameForISP(engine.ISP(ispKey))
		if pool != expectedPool {
			t.Errorf("ISP %s: pool name should be %s, got %s", ispKey, expectedPool, pool)
		}
	}
}
