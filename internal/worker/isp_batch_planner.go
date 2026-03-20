package worker

import (
	"strings"

	"github.com/ignite/sparkpost-monitor/internal/pkg/isp"
)

// ClassifySubscriberISP returns the ISP identifier for an email address.
// Delegates to the canonical isp.Group classifier.
func ClassifySubscriberISP(email string) string {
	return isp.Group(email)
}

// ISPPoolSuffix returns the PMTA pool name suffix for a given ISP classification.
// Delegates to the canonical isp.PoolSuffix.
func ISPPoolSuffix(ispName string) string {
	return isp.PoolSuffix(ispName)
}

// extractPoolSuffix parses the ISP suffix from a pool name.
// "db-gmail-pool" → "gmail", "ht-msft-pool" → "msft".
// Returns "" for legacy pools like "warmup-pool" that don't follow the prefix-isp-pool pattern.
func extractPoolSuffix(poolName string) string {
	if !strings.HasSuffix(poolName, "-pool") {
		return ""
	}
	trimmed := strings.TrimSuffix(poolName, "-pool")
	dashIdx := strings.Index(trimmed, "-")
	if dashIdx < 0 || dashIdx >= len(trimmed)-1 {
		return ""
	}
	return trimmed[dashIdx+1:]
}

// ComputeBatchPlan divides ISP quotas evenly across a fixed number of
// batches. Each ISP gets ceil(quota / numBatches) per batch, with a
// floor of 1 for any ISP that has a non-zero quota.
func ComputeBatchPlan(quotas map[string]int, numBatches int) map[string]int {
	plan := make(map[string]int, len(quotas))
	if numBatches <= 0 {
		for isp := range quotas {
			plan[isp] = 0
		}
		return plan
	}
	for isp, quota := range quotas {
		if quota <= 0 {
			plan[isp] = 0
			continue
		}
		perBatch := (quota + numBatches - 1) / numBatches // ceil division
		if perBatch < 1 {
			perBatch = 1
		}
		plan[isp] = perBatch
	}
	return plan
}

// AssembleBatch determines exactly how many items to pull per ISP for a
// single batch, given per-batch targets and remaining counts. When an
// ISP is exhausted its count drops to 0; other ISPs are unaffected.
func AssembleBatch(targets map[string]int, remaining map[string]int) map[string]int {
	batch := make(map[string]int, len(targets))
	for isp, target := range targets {
		rem := remaining[isp]
		if rem <= 0 || target <= 0 {
			batch[isp] = 0
			continue
		}
		if rem < target {
			batch[isp] = rem
		} else {
			batch[isp] = target
		}
	}
	return batch
}

// BatchTotal returns the sum of all ISP counts in a batch.
func BatchTotal(batch map[string]int) int {
	total := 0
	for _, n := range batch {
		total += n
	}
	return total
}
