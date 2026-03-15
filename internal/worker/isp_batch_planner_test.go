package worker

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// ---------------------------------------------------------------------------
// ClassifySubscriberISP
// ---------------------------------------------------------------------------

func TestClassifySubscriberISP(t *testing.T) {
	tests := []struct {
		email string
		want  string
	}{
		{"user@gmail.com", "gmail"},
		{"user@googlemail.com", "gmail"},
		{"user@yahoo.com", "yahoo"},
		{"user@aol.com", "yahoo"},
		{"user@ymail.com", "yahoo"},
		{"user@outlook.com", "microsoft"},
		{"user@hotmail.com", "microsoft"},
		{"user@live.com", "microsoft"},
		{"user@msn.com", "microsoft"},
		{"user@icloud.com", "apple"},
		{"user@me.com", "apple"},
		{"user@mac.com", "apple"},
		{"user@comcast.net", "comcast"},
		{"user@xfinity.com", "comcast"},
		{"user@att.net", "att"},
		{"user@sbcglobal.net", "att"},
		{"user@bellsouth.net", "att"},
		{"user@cox.net", "cox"},
		{"user@charter.net", "charter"},
		{"user@spectrum.net", "charter"},
		{"user@verizon.net", "verizon"},
		{"user@protonmail.com", "protonmail"},
		{"user@proton.me", "protonmail"},
		{"user@zoho.com", "zoho"},
		{"user@aim.com", "yahoo"},
		{"user@aol.co.uk", "other"},
		{"user@hotmail.co.uk", "other"},
		{"user@live.co.uk", "other"},
		{"user@yahoo.de", "other"},
		{"user@yahoo.fr", "other"},
		{"user@yahoo.co.uk", "other"},
		{"user@yahoo.co.jp", "other"},
		{"user@randomdomain.xyz", "other"},
		{"user@GMAIL.COM", "gmail"},
		{"  user@Yahoo.Ca  ", "yahoo"},
		{"", "other"},
		{"no-at-sign", "other"},
		{"trailing@", "other"},
	}
	for _, tt := range tests {
		t.Run(tt.email, func(t *testing.T) {
			assert.Equal(t, tt.want, ClassifySubscriberISP(tt.email))
		})
	}
}

// ---------------------------------------------------------------------------
// ComputeBatchPlan
// ---------------------------------------------------------------------------

func TestComputeBatchPlan(t *testing.T) {
	tests := []struct {
		name       string
		quotas     map[string]int
		numBatches int
		want       map[string]int
	}{
		{
			name:       "uniform quotas divide evenly",
			quotas:     map[string]int{"gmail": 100, "yahoo": 100, "att": 100},
			numBatches: 10,
			want:       map[string]int{"gmail": 10, "yahoo": 10, "att": 10},
		},
		{
			name:       "uneven quota gets proportional rate",
			quotas:     map[string]int{"gmail": 100, "yahoo": 100, "cox": 80},
			numBatches: 10,
			want:       map[string]int{"gmail": 10, "yahoo": 10, "cox": 8},
		},
		{
			name:       "small quota rounds up to at least 1",
			quotas:     map[string]int{"gmail": 100, "verizon": 3},
			numBatches: 10,
			want:       map[string]int{"gmail": 10, "verizon": 1},
		},
		{
			name:       "zero quota ISP stays zero",
			quotas:     map[string]int{"gmail": 100, "charter": 0},
			numBatches: 10,
			want:       map[string]int{"gmail": 10, "charter": 0},
		},
		{
			name:       "single batch gets full quota",
			quotas:     map[string]int{"gmail": 100, "yahoo": 80},
			numBatches: 1,
			want:       map[string]int{"gmail": 100, "yahoo": 80},
		},
		{
			name:       "zero batches produces all zeros",
			quotas:     map[string]int{"gmail": 100},
			numBatches: 0,
			want:       map[string]int{"gmail": 0},
		},
		{
			name:       "quota less than batches yields 1 per batch",
			quotas:     map[string]int{"gmail": 100, "cox": 5},
			numBatches: 10,
			want:       map[string]int{"gmail": 10, "cox": 1},
		},
		{
			name:       "non-divisible quota rounds up",
			quotas:     map[string]int{"gmail": 97},
			numBatches: 10,
			want:       map[string]int{"gmail": 10},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ComputeBatchPlan(tt.quotas, tt.numBatches)
			assert.Equal(t, tt.want, got)
		})
	}
}

// ---------------------------------------------------------------------------
// AssembleBatch
// ---------------------------------------------------------------------------

func TestAssembleBatch(t *testing.T) {
	tests := []struct {
		name      string
		targets   map[string]int
		remaining map[string]int
		want      map[string]int
	}{
		{
			name:      "all ISPs have full capacity",
			targets:   map[string]int{"gmail": 10, "yahoo": 10, "cox": 8},
			remaining: map[string]int{"gmail": 100, "yahoo": 100, "cox": 80},
			want:      map[string]int{"gmail": 10, "yahoo": 10, "cox": 8},
		},
		{
			name:      "one ISP exhausted — others unchanged",
			targets:   map[string]int{"gmail": 10, "yahoo": 10, "cox": 8},
			remaining: map[string]int{"gmail": 30, "yahoo": 30, "cox": 0},
			want:      map[string]int{"gmail": 10, "yahoo": 10, "cox": 0},
		},
		{
			name:      "one ISP partially remaining",
			targets:   map[string]int{"gmail": 10, "yahoo": 10, "cox": 8},
			remaining: map[string]int{"gmail": 30, "yahoo": 30, "cox": 3},
			want:      map[string]int{"gmail": 10, "yahoo": 10, "cox": 3},
		},
		{
			name:      "all ISPs exhausted — empty batch",
			targets:   map[string]int{"gmail": 10, "yahoo": 10},
			remaining: map[string]int{"gmail": 0, "yahoo": 0},
			want:      map[string]int{"gmail": 0, "yahoo": 0},
		},
		{
			name:      "ISP not in remaining map treated as zero",
			targets:   map[string]int{"gmail": 10, "yahoo": 10, "cox": 8},
			remaining: map[string]int{"gmail": 50, "yahoo": 50},
			want:      map[string]int{"gmail": 10, "yahoo": 10, "cox": 0},
		},
		{
			name:      "multiple ISPs exhaust at different rates",
			targets:   map[string]int{"gmail": 10, "att": 10, "cox": 8, "verizon": 3},
			remaining: map[string]int{"gmail": 50, "att": 5, "cox": 0, "verizon": 1},
			want:      map[string]int{"gmail": 10, "att": 5, "cox": 0, "verizon": 1},
		},
		{
			name:      "negative remaining treated as zero",
			targets:   map[string]int{"gmail": 10},
			remaining: map[string]int{"gmail": -1},
			want:      map[string]int{"gmail": 0},
		},
		{
			name:      "zero target produces zero regardless of remaining",
			targets:   map[string]int{"gmail": 0},
			remaining: map[string]int{"gmail": 999},
			want:      map[string]int{"gmail": 0},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AssembleBatch(tt.targets, tt.remaining)
			assert.Equal(t, tt.want, got)
		})
	}
}

// ---------------------------------------------------------------------------
// Full campaign lifecycle simulations
// ---------------------------------------------------------------------------

func TestLifecycle_UserExample(t *testing.T) {
	// Exact scenario from the user: yahoo=100, gmail=100, att=100, aol=100, cox=80
	// 10 batches. COX gets 8/batch, exhausts at batch 10. Others get 10/batch.
	quotas := map[string]int{
		"yahoo": 100, "gmail": 100, "att": 100, "aol": 100, "cox": 80,
	}
	numBatches := 10
	plan := ComputeBatchPlan(quotas, numBatches)

	remaining := make(map[string]int, len(quotas))
	for k, v := range quotas {
		remaining[k] = v
	}
	totalSent := make(map[string]int)
	var batches []map[string]int

	for i := 0; i < numBatches; i++ {
		batch := AssembleBatch(plan, remaining)
		for isp, count := range batch {
			totalSent[isp] += count
			remaining[isp] -= count
		}
		batches = append(batches, batch)
	}

	// Every ISP received exactly its full quota
	for isp, quota := range quotas {
		assert.Equal(t, quota, totalSent[isp], "ISP %s total mismatch", isp)
	}

	// No remaining should be negative
	for isp, rem := range remaining {
		assert.GreaterOrEqual(t, rem, 0, "ISP %s remaining negative", isp)
	}

	// Per-ISP rate is constant across all 10 batches
	for i := 0; i < numBatches; i++ {
		for _, isp := range []string{"yahoo", "gmail", "att", "aol"} {
			assert.Equal(t, 10, batches[i][isp],
				"batch %d: %s should be 10", i+1, isp)
		}
		assert.Equal(t, 8, batches[i]["cox"],
			"batch %d: cox should be 8", i+1)
	}

	// Batch total is consistent: 10+10+10+10+8 = 48
	for i := 0; i < numBatches; i++ {
		assert.Equal(t, 48, BatchTotal(batches[i]),
			"batch %d total should be 48", i+1)
	}
}

func TestLifecycle_MidCampaignExhaustion(t *testing.T) {
	// COX has 50, others have 100. 10 batches.
	// COX gets ceil(50/10) = 5/batch. Exhausts at batch 10.
	quotas := map[string]int{"gmail": 100, "yahoo": 100, "cox": 50}
	numBatches := 10
	plan := ComputeBatchPlan(quotas, numBatches)

	remaining := make(map[string]int, len(quotas))
	for k, v := range quotas {
		remaining[k] = v
	}
	totalSent := make(map[string]int)
	var batches []map[string]int

	for i := 0; i < numBatches; i++ {
		batch := AssembleBatch(plan, remaining)
		for isp, count := range batch {
			totalSent[isp] += count
			remaining[isp] -= count
		}
		batches = append(batches, batch)
	}

	assert.Equal(t, 100, totalSent["gmail"])
	assert.Equal(t, 100, totalSent["yahoo"])
	assert.Equal(t, 50, totalSent["cox"])

	for i := 0; i < 10; i++ {
		assert.Equal(t, 10, batches[i]["gmail"])
		assert.Equal(t, 10, batches[i]["yahoo"])
		assert.Equal(t, 5, batches[i]["cox"])
	}
}

func TestLifecycle_EarlyExhaustion(t *testing.T) {
	// Verizon has 15, gmail has 100. 10 batches.
	// Verizon gets ceil(15/10) = 2/batch. After 7 batches sent 14,
	// batch 8 sends remaining 1, batches 9-10 send 0.
	quotas := map[string]int{"gmail": 100, "verizon": 15}
	numBatches := 10
	plan := ComputeBatchPlan(quotas, numBatches)

	assert.Equal(t, 10, plan["gmail"])
	assert.Equal(t, 2, plan["verizon"])

	remaining := make(map[string]int, len(quotas))
	for k, v := range quotas {
		remaining[k] = v
	}
	totalSent := make(map[string]int)
	var batches []map[string]int

	for i := 0; i < numBatches; i++ {
		batch := AssembleBatch(plan, remaining)
		for isp, count := range batch {
			totalSent[isp] += count
			remaining[isp] -= count
		}
		batches = append(batches, batch)
	}

	assert.Equal(t, 100, totalSent["gmail"])
	assert.Equal(t, 15, totalSent["verizon"])

	// Gmail rate never changes
	for i := 0; i < 10; i++ {
		assert.Equal(t, 10, batches[i]["gmail"],
			"gmail rate must be constant regardless of verizon state")
	}

	// Verizon: 2 per batch for 7 batches (14), then 1, then 0, then 0
	for i := 0; i < 7; i++ {
		assert.Equal(t, 2, batches[i]["verizon"], "batch %d", i+1)
	}
	assert.Equal(t, 1, batches[7]["verizon"], "batch 8: partial remainder")
	assert.Equal(t, 0, batches[8]["verizon"], "batch 9: exhausted")
	assert.Equal(t, 0, batches[9]["verizon"], "batch 10: exhausted")
}

func TestLifecycle_SingleISP(t *testing.T) {
	quotas := map[string]int{"gmail": 50}
	numBatches := 5
	plan := ComputeBatchPlan(quotas, numBatches)

	remaining := map[string]int{"gmail": 50}
	total := 0

	for i := 0; i < numBatches; i++ {
		batch := AssembleBatch(plan, remaining)
		assert.Equal(t, 10, batch["gmail"])
		total += batch["gmail"]
		remaining["gmail"] -= batch["gmail"]
	}

	assert.Equal(t, 50, total)
	assert.Equal(t, 0, remaining["gmail"])
}

func TestLifecycle_NonDivisibleQuota(t *testing.T) {
	// gmail=97, 10 batches. per_batch = ceil(97/10) = 10.
	// 10*9 = 90, batch 10 sends remaining 7. Total must be exactly 97.
	quotas := map[string]int{"gmail": 97}
	numBatches := 10
	plan := ComputeBatchPlan(quotas, numBatches)
	assert.Equal(t, 10, plan["gmail"])

	remaining := map[string]int{"gmail": 97}
	totalSent := 0
	var batchSizes []int

	for i := 0; i < numBatches; i++ {
		batch := AssembleBatch(plan, remaining)
		totalSent += batch["gmail"]
		remaining["gmail"] -= batch["gmail"]
		batchSizes = append(batchSizes, batch["gmail"])
	}

	assert.Equal(t, 97, totalSent, "total must match quota exactly, not overshoot")
	assert.Equal(t, 0, remaining["gmail"], "nothing remaining")

	// First 9 batches send 10, last sends 7
	for i := 0; i < 9; i++ {
		assert.Equal(t, 10, batchSizes[i], "batch %d", i+1)
	}
	assert.Equal(t, 7, batchSizes[9], "batch 10: remainder")
}

func TestLifecycle_MultiISPNonDivisible(t *testing.T) {
	// Realistic warmup: mixed non-divisible quotas
	quotas := map[string]int{"gmail": 103, "yahoo": 47, "att": 25}
	numBatches := 10
	plan := ComputeBatchPlan(quotas, numBatches)

	assert.Equal(t, 11, plan["gmail"])  // ceil(103/10)
	assert.Equal(t, 5, plan["yahoo"])   // ceil(47/10)
	assert.Equal(t, 3, plan["att"])     // ceil(25/10)

	remaining := make(map[string]int, len(quotas))
	for k, v := range quotas {
		remaining[k] = v
	}
	totalSent := make(map[string]int)

	for i := 0; i < numBatches; i++ {
		batch := AssembleBatch(plan, remaining)
		for isp, count := range batch {
			totalSent[isp] += count
			remaining[isp] -= count
		}
	}

	for isp, quota := range quotas {
		assert.Equal(t, quota, totalSent[isp], "ISP %s must match quota exactly", isp)
		assert.Equal(t, 0, remaining[isp], "ISP %s must have 0 remaining", isp)
	}
}

func TestBatchTotal(t *testing.T) {
	assert.Equal(t, 0, BatchTotal(map[string]int{}))
	assert.Equal(t, 10, BatchTotal(map[string]int{"gmail": 10}))
	assert.Equal(t, 28, BatchTotal(map[string]int{"gmail": 10, "yahoo": 10, "cox": 8}))
}
