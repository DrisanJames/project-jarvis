//go:build ignore
// +build ignore

// Suppression Benchmark Tool
// Tests how quickly we can suppress users from a target audience against a large suppression file.
//
// Usage:
//   go run scripts/suppression_benchmark.go \
//     --suppression-size=10000000 \
//     --audience-size=5000000 \
//     --workers=16
//
// Or with actual files:
//   go run scripts/suppression_benchmark.go \
//     --suppression-file=/path/to/suppression.csv \
//     --audience-file=/path/to/audience.csv \
//     --workers=16

package main

import (
	"bufio"
	"crypto/md5"
	"encoding/csv"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// =============================================================================
// CONFIGURATION
// =============================================================================

type BenchmarkConfig struct {
	// Synthetic test sizes
	SuppressionSize int64
	AudienceSize    int64

	// Real file paths (optional)
	SuppressionFile string
	AudienceFile    string

	// Processing config
	Workers       int
	BatchSize     int
	WarmupBatches int

	// Suppression overlap (for synthetic data)
	OverlapPercentage float64 // What % of audience should be suppressed
}

func DefaultBenchmarkConfig() *BenchmarkConfig {
	return &BenchmarkConfig{
		SuppressionSize:   10_000_000, // 10M suppression records
		AudienceSize:      5_000_000,  // 5M target audience
		Workers:           runtime.NumCPU(),
		BatchSize:         10_000,
		WarmupBatches:     5,
		OverlapPercentage: 0.15, // 15% of audience is suppressed
	}
}

// =============================================================================
// MD5 HASH TYPE (matching suppression engine)
// =============================================================================

type MD5Hash [16]byte

func MD5HashFromEmail(email string) MD5Hash {
	normalized := strings.ToLower(strings.TrimSpace(email))
	return md5.Sum([]byte(normalized))
}

func MD5HashFromHex(hexStr string) (MD5Hash, error) {
	var h MD5Hash
	hexStr = strings.ToLower(strings.TrimSpace(hexStr))
	if len(hexStr) != 32 {
		return h, fmt.Errorf("invalid MD5 hex length: %d", len(hexStr))
	}
	_, err := hex.Decode(h[:], []byte(hexStr))
	return h, err
}

func (h MD5Hash) Compare(other MD5Hash) int {
	for i := 0; i < 16; i++ {
		if h[i] < other[i] {
			return -1
		}
		if h[i] > other[i] {
			return 1
		}
	}
	return 0
}

// =============================================================================
// BLOOM FILTER (optimized for suppression)
// =============================================================================

type BloomFilter struct {
	bits      []uint64
	size      uint64
	hashCount uint
}

func NewBloomFilter(expectedElements uint64, fpRate float64) *BloomFilter {
	// m = -n * ln(p) / (ln(2)^2)
	m := uint64(float64(expectedElements) * (-ln(fpRate)) / 0.4804530139182015)
	if m < 64 {
		m = 64
	}
	m = ((m + 63) / 64) * 64

	// k = (m/n) * ln(2)
	k := uint((float64(m) / float64(expectedElements)) * 0.6931471805599453)
	if k < 1 {
		k = 1
	}
	if k > 16 {
		k = 16
	}

	return &BloomFilter{
		bits:      make([]uint64, m/64),
		size:      m,
		hashCount: k,
	}
}

func (bf *BloomFilter) Add(h MD5Hash) {
	for i := uint(0); i < bf.hashCount; i++ {
		pos := bf.hash(h, i) % bf.size
		bf.bits[pos/64] |= 1 << (pos % 64)
	}
}

func (bf *BloomFilter) MayContain(h MD5Hash) bool {
	for i := uint(0); i < bf.hashCount; i++ {
		pos := bf.hash(h, i) % bf.size
		if bf.bits[pos/64]&(1<<(pos%64)) == 0 {
			return false
		}
	}
	return true
}

func (bf *BloomFilter) hash(h MD5Hash, i uint) uint64 {
	h1 := uint64(h[0]) | uint64(h[1])<<8 | uint64(h[2])<<16 | uint64(h[3])<<24 |
		uint64(h[4])<<32 | uint64(h[5])<<40 | uint64(h[6])<<48 | uint64(h[7])<<56
	h2 := uint64(h[8]) | uint64(h[9])<<8 | uint64(h[10])<<16 | uint64(h[11])<<24 |
		uint64(h[12])<<32 | uint64(h[13])<<40 | uint64(h[14])<<48 | uint64(h[15])<<56
	return h1 + uint64(i)*h2
}

func (bf *BloomFilter) MemoryBytes() uint64 {
	return uint64(len(bf.bits)) * 8
}

func ln(x float64) float64 {
	if x <= 0 {
		return -1e100
	}
	n := 0.0
	for x > 2 {
		x /= 2.718281828459045
		n++
	}
	for x < 0.5 {
		x *= 2.718281828459045
		n--
	}
	y := (x - 1) / (x + 1)
	y2 := y * y
	result := y
	term := y
	for i := 3; i < 50; i += 2 {
		term *= y2
		result += term / float64(i)
	}
	return 2*result + n
}

// =============================================================================
// SUPPRESSION ENGINE (simplified for benchmark)
// =============================================================================

type SuppressionEngine struct {
	bloom  *BloomFilter
	hashes []MD5Hash // Sorted for binary search
	count  int64
}

func NewSuppressionEngine(hashes []MD5Hash) *SuppressionEngine {
	// Deduplicate and sort
	unique := deduplicateAndSort(hashes)

	// Create bloom filter
	bloom := NewBloomFilter(uint64(len(unique)), 0.001) // 0.1% FP rate
	for _, h := range unique {
		bloom.Add(h)
	}

	return &SuppressionEngine{
		bloom:  bloom,
		hashes: unique,
		count:  int64(len(unique)),
	}
}

func (e *SuppressionEngine) IsSuppressed(h MD5Hash) bool {
	// Layer 1: Bloom filter
	if !e.bloom.MayContain(h) {
		return false
	}

	// Layer 2: Binary search verification
	return binarySearch(e.hashes, h)
}

func (e *SuppressionEngine) Count() int64 {
	return e.count
}

func (e *SuppressionEngine) MemoryBytes() uint64 {
	return e.bloom.MemoryBytes() + uint64(len(e.hashes))*16
}

func binarySearch(hashes []MD5Hash, target MD5Hash) bool {
	left, right := 0, len(hashes)-1
	for left <= right {
		mid := left + (right-left)/2
		cmp := target.Compare(hashes[mid])
		if cmp == 0 {
			return true
		} else if cmp < 0 {
			right = mid - 1
		} else {
			left = mid + 1
		}
	}
	return false
}

func deduplicateAndSort(hashes []MD5Hash) []MD5Hash {
	if len(hashes) == 0 {
		return hashes
	}

	sort.Slice(hashes, func(i, j int) bool {
		return hashes[i].Compare(hashes[j]) < 0
	})

	unique := hashes[:1]
	for i := 1; i < len(hashes); i++ {
		if hashes[i].Compare(unique[len(unique)-1]) != 0 {
			unique = append(unique, hashes[i])
		}
	}
	return unique
}

// =============================================================================
// BENCHMARK METRICS
// =============================================================================

type BenchmarkMetrics struct {
	mu sync.Mutex

	// Loading metrics
	SuppressionLoadTime   time.Duration
	SuppressionCount      int64
	SuppressionMemoryMB   float64
	AudienceLoadTime      time.Duration
	AudienceCount         int64

	// Processing metrics
	TotalChecked      int64
	TotalSuppressed   int64
	TotalDeliverable  int64
	ProcessingTime    time.Duration
	ChecksPerSecond   float64
	LatenciesNs       []int64 // Sample latencies

	// Batch metrics
	BatchCount        int
	BatchLatencyP50   time.Duration
	BatchLatencyP95   time.Duration
	BatchLatencyP99   time.Duration

	// Bloom filter stats
	BloomFalsePositives int64
	BloomTruePositives  int64
}

func (m *BenchmarkMetrics) RecordLatency(ns int64) {
	m.mu.Lock()
	// Sample 1% of latencies to avoid memory explosion
	if len(m.LatenciesNs) < 100000 && rand.Float32() < 0.01 {
		m.LatenciesNs = append(m.LatenciesNs, ns)
	}
	m.mu.Unlock()
}

func (m *BenchmarkMetrics) Finalize() {
	if len(m.LatenciesNs) > 0 {
		sort.Slice(m.LatenciesNs, func(i, j int) bool {
			return m.LatenciesNs[i] < m.LatenciesNs[j]
		})
		m.BatchLatencyP50 = time.Duration(percentile(m.LatenciesNs, 0.50))
		m.BatchLatencyP95 = time.Duration(percentile(m.LatenciesNs, 0.95))
		m.BatchLatencyP99 = time.Duration(percentile(m.LatenciesNs, 0.99))
	}

	if m.ProcessingTime > 0 {
		m.ChecksPerSecond = float64(m.TotalChecked) / m.ProcessingTime.Seconds()
	}
}

func percentile(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * p)
	return sorted[idx]
}

// =============================================================================
// BENCHMARK RUNNER
// =============================================================================

type BenchmarkRunner struct {
	config  *BenchmarkConfig
	engine  *SuppressionEngine
	metrics *BenchmarkMetrics
}

func NewBenchmarkRunner(cfg *BenchmarkConfig) *BenchmarkRunner {
	return &BenchmarkRunner{
		config:  cfg,
		metrics: &BenchmarkMetrics{},
	}
}

func (r *BenchmarkRunner) Run() error {
	fmt.Println("================================================================================")
	fmt.Println("              SUPPRESSION PERFORMANCE BENCHMARK")
	fmt.Println("================================================================================")
	fmt.Printf("\nConfiguration:\n")
	fmt.Printf("  Suppression Size:   %d records\n", r.config.SuppressionSize)
	fmt.Printf("  Audience Size:      %d records\n", r.config.AudienceSize)
	fmt.Printf("  Workers:            %d\n", r.config.Workers)
	fmt.Printf("  Batch Size:         %d\n", r.config.BatchSize)
	fmt.Printf("  Overlap:            %.1f%%\n", r.config.OverlapPercentage*100)
	fmt.Println()

	// Phase 1: Load suppression list
	fmt.Println("PHASE 1: LOADING SUPPRESSION LIST")
	fmt.Println("-" + strings.Repeat("-", 78))

	var suppressionHashes []MD5Hash
	var err error

	if r.config.SuppressionFile != "" {
		fmt.Printf("  Loading from file: %s\n", r.config.SuppressionFile)
		suppressionHashes, err = r.loadHashesFromFile(r.config.SuppressionFile)
	} else {
		fmt.Printf("  Generating %d synthetic suppression records...\n", r.config.SuppressionSize)
		suppressionHashes, err = r.generateSyntheticHashes(r.config.SuppressionSize)
	}
	if err != nil {
		return fmt.Errorf("failed to load suppression list: %w", err)
	}

	start := time.Now()
	r.engine = NewSuppressionEngine(suppressionHashes)
	r.metrics.SuppressionLoadTime = time.Since(start)
	r.metrics.SuppressionCount = r.engine.Count()
	r.metrics.SuppressionMemoryMB = float64(r.engine.MemoryBytes()) / (1024 * 1024)

	fmt.Printf("  ✓ Loaded %d unique records in %v\n", r.metrics.SuppressionCount, r.metrics.SuppressionLoadTime)
	fmt.Printf("  ✓ Memory usage: %.2f MB (Bloom + Binary MD5s)\n", r.metrics.SuppressionMemoryMB)
	fmt.Printf("  ✓ Load rate: %.2f records/second\n", float64(r.metrics.SuppressionCount)/r.metrics.SuppressionLoadTime.Seconds())
	fmt.Println()

	// Phase 2: Load/generate audience
	fmt.Println("PHASE 2: LOADING TARGET AUDIENCE")
	fmt.Println("-" + strings.Repeat("-", 78))

	var audienceHashes []MD5Hash
	if r.config.AudienceFile != "" {
		fmt.Printf("  Loading from file: %s\n", r.config.AudienceFile)
		audienceHashes, err = r.loadHashesFromFile(r.config.AudienceFile)
	} else {
		fmt.Printf("  Generating %d synthetic audience records...\n", r.config.AudienceSize)
		audienceHashes, err = r.generateAudienceWithOverlap(r.config.AudienceSize, suppressionHashes)
	}
	if err != nil {
		return fmt.Errorf("failed to load audience: %w", err)
	}

	r.metrics.AudienceCount = int64(len(audienceHashes))
	fmt.Printf("  ✓ Loaded %d audience records\n", r.metrics.AudienceCount)
	fmt.Println()

	// Phase 3: Warmup
	fmt.Println("PHASE 3: WARMUP")
	fmt.Println("-" + strings.Repeat("-", 78))
	r.runWarmup(audienceHashes[:min(r.config.BatchSize*r.config.WarmupBatches, len(audienceHashes))])
	fmt.Printf("  ✓ Warmup complete (%d batches)\n", r.config.WarmupBatches)
	fmt.Println()

	// Phase 4: Full benchmark
	fmt.Println("PHASE 4: FULL SUPPRESSION BENCHMARK")
	fmt.Println("-" + strings.Repeat("-", 78))
	r.runFullBenchmark(audienceHashes)

	// Phase 5: Results
	r.metrics.Finalize()
	r.printResults()

	return nil
}

func (r *BenchmarkRunner) loadHashesFromFile(path string) ([]MD5Hash, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	// Detect if CSV or plain text
	buf := bufio.NewReader(file)
	firstLine, err := buf.ReadString('\n')
	if err != nil && err != io.EOF {
		return nil, err
	}

	// Reset file
	file.Seek(0, 0)

	var hashes []MD5Hash

	if strings.Contains(firstLine, ",") {
		// CSV format
		reader := csv.NewReader(file)
		header, err := reader.Read()
		if err != nil {
			return nil, err
		}

		// Find email column
		emailCol := -1
		md5Col := -1
		for i, col := range header {
			col = strings.ToLower(strings.TrimSpace(col))
			if col == "email" || col == "email_address" {
				emailCol = i
			}
			if col == "md5" || col == "md5_hash" || col == "hash" {
				md5Col = i
			}
		}

		for {
			record, err := reader.Read()
			if err == io.EOF {
				break
			}
			if err != nil {
				continue
			}

			if md5Col >= 0 && md5Col < len(record) {
				if h, err := MD5HashFromHex(record[md5Col]); err == nil {
					hashes = append(hashes, h)
				}
			} else if emailCol >= 0 && emailCol < len(record) {
				hashes = append(hashes, MD5HashFromEmail(record[emailCol]))
			}
		}
	} else {
		// Plain text (one email or MD5 per line)
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}

			if len(line) == 32 {
				if h, err := MD5HashFromHex(line); err == nil {
					hashes = append(hashes, h)
				}
			} else if strings.Contains(line, "@") {
				hashes = append(hashes, MD5HashFromEmail(line))
			}
		}
	}

	return hashes, nil
}

func (r *BenchmarkRunner) generateSyntheticHashes(count int64) ([]MD5Hash, error) {
	hashes := make([]MD5Hash, count)
	for i := int64(0); i < count; i++ {
		email := fmt.Sprintf("suppressed%d@domain%d.com", i, i%10000)
		hashes[i] = MD5HashFromEmail(email)

		if i%1000000 == 0 && i > 0 {
			fmt.Printf("    Generated %dM records...\n", i/1000000)
		}
	}
	return hashes, nil
}

func (r *BenchmarkRunner) generateAudienceWithOverlap(count int64, suppressionHashes []MD5Hash) ([]MD5Hash, error) {
	hashes := make([]MD5Hash, count)
	overlapCount := int64(float64(count) * r.config.OverlapPercentage)
	
	// Add overlapping hashes (from suppression list)
	for i := int64(0); i < overlapCount && i < int64(len(suppressionHashes)); i++ {
		idx := rand.Int63n(int64(len(suppressionHashes)))
		hashes[i] = suppressionHashes[idx]
	}

	// Add non-overlapping hashes (unique audience members)
	for i := overlapCount; i < count; i++ {
		email := fmt.Sprintf("audience%d@target%d.com", i, i%5000)
		hashes[i] = MD5HashFromEmail(email)
	}

	// Shuffle to randomize order
	rand.Shuffle(len(hashes), func(i, j int) {
		hashes[i], hashes[j] = hashes[j], hashes[i]
	})

	return hashes, nil
}

func (r *BenchmarkRunner) runWarmup(hashes []MD5Hash) {
	for _, h := range hashes {
		r.engine.IsSuppressed(h)
	}
}

func (r *BenchmarkRunner) runFullBenchmark(audienceHashes []MD5Hash) {
	start := time.Now()

	var totalSuppressed int64
	var totalChecked int64
	var batchLatencies []time.Duration

	// Create worker pool
	batchChan := make(chan []MD5Hash, r.config.Workers*2)
	var wg sync.WaitGroup

	// Start workers
	for w := 0; w < r.config.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for batch := range batchChan {
				batchStart := time.Now()
				suppressed := int64(0)
				for _, h := range batch {
					checkStart := time.Now()
					if r.engine.IsSuppressed(h) {
						suppressed++
					}
					r.metrics.RecordLatency(time.Since(checkStart).Nanoseconds())
				}
				atomic.AddInt64(&totalSuppressed, suppressed)
				atomic.AddInt64(&totalChecked, int64(len(batch)))

				r.metrics.mu.Lock()
				batchLatencies = append(batchLatencies, time.Since(batchStart))
				r.metrics.mu.Unlock()
			}
		}()
	}

	// Send batches
	for i := 0; i < len(audienceHashes); i += r.config.BatchSize {
		end := min(i+r.config.BatchSize, len(audienceHashes))
		batchChan <- audienceHashes[i:end]
	}
	close(batchChan)
	wg.Wait()

	r.metrics.ProcessingTime = time.Since(start)
	r.metrics.TotalChecked = totalChecked
	r.metrics.TotalSuppressed = totalSuppressed
	r.metrics.TotalDeliverable = totalChecked - totalSuppressed
	r.metrics.BatchCount = len(batchLatencies)

	// Calculate batch latency percentiles
	if len(batchLatencies) > 0 {
		sort.Slice(batchLatencies, func(i, j int) bool {
			return batchLatencies[i] < batchLatencies[j]
		})
		r.metrics.BatchLatencyP50 = batchLatencies[int(float64(len(batchLatencies)-1)*0.50)]
		r.metrics.BatchLatencyP95 = batchLatencies[int(float64(len(batchLatencies)-1)*0.95)]
		r.metrics.BatchLatencyP99 = batchLatencies[int(float64(len(batchLatencies)-1)*0.99)]
	}
}

func (r *BenchmarkRunner) printResults() {
	fmt.Println()
	fmt.Println("================================================================================")
	fmt.Println("                         BENCHMARK RESULTS")
	fmt.Println("================================================================================")
	fmt.Println()

	// Summary
	fmt.Println("SUMMARY")
	fmt.Println("-" + strings.Repeat("-", 78))
	fmt.Printf("  Total Audience:       %d records\n", r.metrics.TotalChecked)
	fmt.Printf("  Suppressed:           %d records (%.2f%%)\n",
		r.metrics.TotalSuppressed,
		float64(r.metrics.TotalSuppressed)/float64(r.metrics.TotalChecked)*100)
	fmt.Printf("  Deliverable:          %d records (%.2f%%)\n",
		r.metrics.TotalDeliverable,
		float64(r.metrics.TotalDeliverable)/float64(r.metrics.TotalChecked)*100)
	fmt.Println()

	// Performance
	fmt.Println("PERFORMANCE")
	fmt.Println("-" + strings.Repeat("-", 78))
	fmt.Printf("  Total Processing Time:   %v\n", r.metrics.ProcessingTime)
	fmt.Printf("  Checks Per Second:       %.2f\n", r.metrics.ChecksPerSecond)
	fmt.Printf("  Time Per Check:          %.2f µs\n", float64(r.metrics.ProcessingTime.Microseconds())/float64(r.metrics.TotalChecked))
	fmt.Println()

	// Batch Metrics
	fmt.Println("BATCH METRICS")
	fmt.Println("-" + strings.Repeat("-", 78))
	fmt.Printf("  Batch Count:             %d batches\n", r.metrics.BatchCount)
	fmt.Printf("  Batch Latency P50:       %v\n", r.metrics.BatchLatencyP50)
	fmt.Printf("  Batch Latency P95:       %v\n", r.metrics.BatchLatencyP95)
	fmt.Printf("  Batch Latency P99:       %v\n", r.metrics.BatchLatencyP99)
	fmt.Println()

	// Memory
	fmt.Println("MEMORY USAGE")
	fmt.Println("-" + strings.Repeat("-", 78))
	fmt.Printf("  Suppression List:        %.2f MB\n", r.metrics.SuppressionMemoryMB)
	fmt.Printf("  Bytes per Record:        %.2f bytes\n", float64(r.engine.MemoryBytes())/float64(r.metrics.SuppressionCount))
	fmt.Println()

	// Throughput projections
	fmt.Println("THROUGHPUT PROJECTIONS")
	fmt.Println("-" + strings.Repeat("-", 78))
	checksPerSec := r.metrics.ChecksPerSecond

	// Time to suppress different audience sizes
	audienceSizes := []struct {
		name string
		size int64
	}{
		{"1 Million", 1_000_000},
		{"5 Million", 5_000_000},
		{"10 Million", 10_000_000},
		{"50 Million", 50_000_000},
		{"100 Million", 100_000_000},
	}

	fmt.Printf("  At %.2f checks/sec:\n\n", checksPerSec)
	for _, a := range audienceSizes {
		seconds := float64(a.size) / checksPerSec
		var timeStr string
		if seconds < 60 {
			timeStr = fmt.Sprintf("%.1f seconds", seconds)
		} else if seconds < 3600 {
			timeStr = fmt.Sprintf("%.1f minutes", seconds/60)
		} else {
			timeStr = fmt.Sprintf("%.1f hours", seconds/3600)
		}
		fmt.Printf("    %15s audience: %s\n", a.name, timeStr)
	}
	fmt.Println()

	// Verdict
	fmt.Println("================================================================================")
	if r.metrics.ChecksPerSecond >= 1_000_000 {
		fmt.Println("  RESULT: EXCELLENT - Can suppress 1M+ records per second")
	} else if r.metrics.ChecksPerSecond >= 500_000 {
		fmt.Println("  RESULT: GOOD - Can suppress 500K+ records per second")
	} else if r.metrics.ChecksPerSecond >= 100_000 {
		fmt.Println("  RESULT: ACCEPTABLE - Can suppress 100K+ records per second")
	} else {
		fmt.Println("  RESULT: NEEDS OPTIMIZATION - Below 100K checks per second")
	}
	fmt.Println("================================================================================")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// =============================================================================
// MAIN
// =============================================================================

func main() {
	cfg := DefaultBenchmarkConfig()

	flag.Int64Var(&cfg.SuppressionSize, "suppression-size", cfg.SuppressionSize, "Number of suppression records (synthetic)")
	flag.Int64Var(&cfg.AudienceSize, "audience-size", cfg.AudienceSize, "Number of audience records (synthetic)")
	flag.StringVar(&cfg.SuppressionFile, "suppression-file", "", "Path to suppression file (CSV or text)")
	flag.StringVar(&cfg.AudienceFile, "audience-file", "", "Path to audience file (CSV or text)")
	flag.IntVar(&cfg.Workers, "workers", cfg.Workers, "Number of parallel workers")
	flag.IntVar(&cfg.BatchSize, "batch-size", cfg.BatchSize, "Batch size for processing")
	flag.Float64Var(&cfg.OverlapPercentage, "overlap", cfg.OverlapPercentage, "Percentage of audience that should be suppressed (0.0-1.0)")

	flag.Parse()

	runner := NewBenchmarkRunner(cfg)
	if err := runner.Run(); err != nil {
		log.Fatalf("Benchmark failed: %v", err)
	}
}
