package worker

import (
	"strings"
)

// ISP domain classification map — single source of truth for the entire
// send pipeline. Used by both the enqueue path (to tag queue items) and
// the batch planner (to compute per-ISP rates).
var ispDomainClassification = map[string]string{
	"gmail.com": "gmail", "googlemail.com": "gmail",
	"outlook.com": "microsoft", "hotmail.com": "microsoft", "live.com": "microsoft", "msn.com": "microsoft",
	"yahoo.com": "yahoo", "ymail.com": "yahoo", "rocketmail.com": "yahoo",
	"yahoo.ca": "yahoo",
	"aol.com": "yahoo", "aim.com": "yahoo",
	"icloud.com": "apple", "me.com": "apple", "mac.com": "apple",
	"comcast.net": "comcast", "xfinity.com": "comcast",
	"att.net": "att", "sbcglobal.net": "att", "bellsouth.net": "att",
	"cox.net":     "cox",
	"charter.net": "charter", "spectrum.net": "charter",
	"rr.com": "charter", "roadrunner.com": "charter",
	"twc.com": "charter", "brighthouse.com": "charter",
	"verizon.net":    "verizon",
	"protonmail.com": "protonmail", "proton.me": "protonmail",
	"zoho.com": "zoho",
}

// ClassifySubscriberISP returns the ISP identifier for an email address.
func ClassifySubscriberISP(email string) string {
	lower := strings.ToLower(strings.TrimSpace(email))
	atIdx := strings.LastIndex(lower, "@")
	if atIdx < 0 || atIdx == len(lower)-1 {
		return "other"
	}
	domain := lower[atIdx+1:]
	if isp, ok := ispDomainClassification[domain]; ok {
		return isp
	}
	if strings.HasSuffix(domain, ".rr.com") {
		return "charter"
	}
	return "other"
}

// ispToPoolSuffix maps Go ISP classification names to PMTA pool name suffixes.
// ISPs with dedicated classification but no per-ISP pool route to "general".
var ispToPoolSuffix = map[string]string{
	"gmail":     "gmail",
	"yahoo":     "yahoo",
	"microsoft": "msft",
	"apple":     "apple",
	"comcast":   "comcast",
	"att":       "att",
	"cox":       "cox",
	"charter":   "charter",
}

// ISPPoolSuffix returns the PMTA pool name suffix for a given ISP classification.
// For example, ISPPoolSuffix("microsoft") returns "msft".
// Unmapped ISPs (verizon, protonmail, zoho, other, etc.) return "general".
func ISPPoolSuffix(isp string) string {
	if suffix, ok := ispToPoolSuffix[isp]; ok {
		return suffix
	}
	return "general"
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
