package suppression

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// =============================================================================
// TEST HELPERS
// =============================================================================

func generateTestEmail(i int) string {
	return fmt.Sprintf("user%d@example.com", i)
}

func generateTestMD5(i int) MD5Hash {
	return MD5HashFromEmail(generateTestEmail(i))
}

func generateTestHexMD5(i int) string {
	return generateTestMD5(i).ToHex()
}

// =============================================================================
// MD5Hash TESTS
// =============================================================================

func TestMD5HashFromHex_Valid(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"lowercase", "5d41402abc4b2a76b9719d911017c592", false},
		{"uppercase", "5D41402ABC4B2A76B9719D911017C592", false},
		{"mixed case", "5d41402ABC4b2a76B9719d911017c592", false},
		{"with spaces", "  5d41402abc4b2a76b9719d911017c592  ", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, err := MD5HashFromHex(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("MD5HashFromHex() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && h.ToHex() != strings.ToLower(strings.TrimSpace(tt.input)) {
				t.Errorf("MD5HashFromHex() roundtrip failed")
			}
		})
	}
}

func TestMD5HashFromHex_Invalid(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"too short", "5d41402abc4b2a76"},
		{"too long", "5d41402abc4b2a76b9719d911017c5921234"},
		{"invalid chars", "5d41402abc4b2a76b9719d911017c59g"},
		{"empty", ""},
		{"spaces only", "   "},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := MD5HashFromHex(tt.input)
			if err == nil {
				t.Errorf("MD5HashFromHex() expected error for input %q", tt.input)
			}
		})
	}
}

func TestMD5HashFromEmail(t *testing.T) {
	tests := []struct {
		email    string
		expected string
	}{
		{"test@example.com", hex.EncodeToString(md5.New().Sum([]byte("test@example.com"))[:16])},
		{"TEST@EXAMPLE.COM", hex.EncodeToString(md5.New().Sum([]byte("test@example.com"))[:16])}, // Should normalize
		{"  test@example.com  ", hex.EncodeToString(md5.New().Sum([]byte("test@example.com"))[:16])}, // Should trim
	}

	// Pre-compute expected values properly
	for i := range tests {
		h := md5.Sum([]byte(strings.ToLower(strings.TrimSpace(tests[i].email))))
		tests[i].expected = hex.EncodeToString(h[:])
	}

	for _, tt := range tests {
		t.Run(tt.email, func(t *testing.T) {
			h := MD5HashFromEmail(tt.email)
			if h.ToHex() != tt.expected {
				t.Errorf("MD5HashFromEmail(%q) = %s, want %s", tt.email, h.ToHex(), tt.expected)
			}
		})
	}
}

func TestMD5Hash_Compare(t *testing.T) {
	h1, _ := MD5HashFromHex("00000000000000000000000000000001")
	h2, _ := MD5HashFromHex("00000000000000000000000000000002")
	h1Copy, _ := MD5HashFromHex("00000000000000000000000000000001")

	if h1.Compare(h2) >= 0 {
		t.Error("h1 should be less than h2")
	}
	if h2.Compare(h1) <= 0 {
		t.Error("h2 should be greater than h1")
	}
	if h1.Compare(h1Copy) != 0 {
		t.Error("h1 should equal h1Copy")
	}
}

// =============================================================================
// BLOOM FILTER TESTS
// =============================================================================

func TestBloomFilter_Basic(t *testing.T) {
	cfg := DefaultBloomConfig(1000)
	bf := NewBloomFilter(cfg)

	// Add some hashes
	h1 := MD5HashFromEmail("test1@example.com")
	h2 := MD5HashFromEmail("test2@example.com")

	bf.Add(h1)
	bf.Add(h2)

	// Check membership
	if !bf.MayContain(h1) {
		t.Error("MayContain should return true for h1")
	}
	if !bf.MayContain(h2) {
		t.Error("MayContain should return true for h2")
	}
	
	// Note: Testing false positives is probabilistic, so we skip deterministic testing
	// The false positive rate test covers this more thoroughly

	if bf.Count() != 2 {
		t.Errorf("Count() = %d, want 2", bf.Count())
	}
}

func TestBloomFilter_NoFalseNegatives(t *testing.T) {
	cfg := DefaultBloomConfig(10000)
	bf := NewBloomFilter(cfg)

	// Add 10000 hashes
	hashes := make([]MD5Hash, 10000)
	for i := 0; i < 10000; i++ {
		hashes[i] = generateTestMD5(i)
		bf.Add(hashes[i])
	}

	// Verify ALL added elements are found (no false negatives)
	for i, h := range hashes {
		if !bf.MayContain(h) {
			t.Errorf("False negative detected at index %d", i)
		}
	}
}

func TestBloomFilter_FalsePositiveRate(t *testing.T) {
	// Test that false positive rate is within expected bounds
	expectedElements := uint64(100000)
	cfg := BloomFilterConfig{
		ExpectedElements:  expectedElements,
		FalsePositiveRate: 0.01, // 1%
	}
	bf := NewBloomFilter(cfg)

	// Add elements
	for i := uint64(0); i < expectedElements; i++ {
		bf.Add(generateTestMD5(int(i)))
	}

	// Check false positive rate with elements NOT in the set
	falsePositives := 0
	testCount := 100000
	for i := 0; i < testCount; i++ {
		h := generateTestMD5(int(expectedElements) + i + 1000000)
		if bf.MayContain(h) {
			falsePositives++
		}
	}

	actualRate := float64(falsePositives) / float64(testCount)
	
	// Allow 2x the expected rate as tolerance
	if actualRate > 0.02 {
		t.Errorf("False positive rate too high: got %.4f, want < 0.02", actualRate)
	}
}

func TestBloomFilter_MemoryEfficiency(t *testing.T) {
	cfg := BloomFilterConfig{
		ExpectedElements:  1000000, // 1M elements
		FalsePositiveRate: 0.001,   // 0.1%
	}
	bf := NewBloomFilter(cfg)

	memBytes := bf.MemoryBytes()
	memMB := float64(memBytes) / (1024 * 1024)

	// For 1M elements at 0.1% FP rate, expect ~1.8MB (14.4 bits per element)
	// Allow up to 3MB as reasonable
	if memMB > 3 {
		t.Errorf("Memory usage too high: %.2f MB, want < 3 MB", memMB)
	}
}

// =============================================================================
// SUPPRESSION LIST TESTS
// =============================================================================

func TestSuppressionList_Basic(t *testing.T) {
	hashes := []MD5Hash{
		MD5HashFromEmail("suppress1@example.com"),
		MD5HashFromEmail("suppress2@example.com"),
		MD5HashFromEmail("suppress3@example.com"),
	}

	list, err := NewSuppressionList("test-list", "Test List", "manual", hashes)
	if err != nil {
		t.Fatalf("NewSuppressionList() error = %v", err)
	}

	// Test Contains
	if !list.Contains(hashes[0]) {
		t.Error("Contains should return true for added hash")
	}
	if !list.ContainsEmail("suppress1@example.com") {
		t.Error("ContainsEmail should return true for suppressed email")
	}
	if list.ContainsEmail("notsuppressed@example.com") {
		t.Error("ContainsEmail should return false for non-suppressed email")
	}

	// Test Count
	if list.Count() != 3 {
		t.Errorf("Count() = %d, want 3", list.Count())
	}
}

func TestSuppressionList_Deduplication(t *testing.T) {
	h := MD5HashFromEmail("duplicate@example.com")
	hashes := []MD5Hash{h, h, h, h, h} // 5 duplicates

	list, err := NewSuppressionList("dedup-test", "Dedup Test", "manual", hashes)
	if err != nil {
		t.Fatalf("NewSuppressionList() error = %v", err)
	}

	if list.Count() != 1 {
		t.Errorf("Count() = %d, want 1 after deduplication", list.Count())
	}
}

func TestSuppressionList_EmptyList(t *testing.T) {
	_, err := NewSuppressionList("empty", "Empty", "manual", []MD5Hash{})
	if err != ErrEmptyList {
		t.Errorf("Expected ErrEmptyList, got %v", err)
	}
}

func TestSuppressionList_LargeList(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping large list test in short mode")
	}

	// Test with 100K entries
	count := 100000
	hashes := make([]MD5Hash, count)
	for i := 0; i < count; i++ {
		hashes[i] = generateTestMD5(i)
	}

	start := time.Now()
	list, err := NewSuppressionList("large-list", "Large List", "test", hashes)
	loadTime := time.Since(start)

	if err != nil {
		t.Fatalf("NewSuppressionList() error = %v", err)
	}

	t.Logf("Loaded %d entries in %v", count, loadTime)

	// Verify all entries are found
	start = time.Now()
	for i := 0; i < count; i++ {
		if !list.Contains(hashes[i]) {
			t.Errorf("Entry %d not found", i)
		}
	}
	checkTime := time.Since(start)
	t.Logf("Checked %d entries in %v (%.2f µs/check)", count, checkTime, float64(checkTime.Microseconds())/float64(count))

	// Memory stats
	stats := list.Stats()
	t.Logf("Memory: Bloom=%.2f KB, Hashes=%.2f KB, Total=%.2f KB",
		float64(stats.BloomMemoryBytes)/1024,
		float64(stats.HashArrayMemoryBytes)/1024,
		float64(stats.TotalMemoryBytes)/1024)
}

// =============================================================================
// MANAGER TESTS
// =============================================================================

func TestManager_Basic(t *testing.T) {
	m := NewManager()

	hashes := []MD5Hash{
		MD5HashFromEmail("blocked@example.com"),
	}

	list, err := m.LoadList("list1", "List 1", "test", hashes)
	if err != nil {
		t.Fatalf("LoadList() error = %v", err)
	}

	if list.ID != "list1" {
		t.Errorf("List ID = %s, want list1", list.ID)
	}

	// Test IsSuppressed
	if !m.IsSuppressed("blocked@example.com", []string{"list1"}) {
		t.Error("IsSuppressed should return true for blocked email")
	}
	if m.IsSuppressed("allowed@example.com", []string{"list1"}) {
		t.Error("IsSuppressed should return false for allowed email")
	}
}

func TestManager_MultipleLists(t *testing.T) {
	m := NewManager()

	// Load two lists
	m.LoadList("list1", "List 1", "test", []MD5Hash{MD5HashFromEmail("user1@example.com")})
	m.LoadList("list2", "List 2", "test", []MD5Hash{MD5HashFromEmail("user2@example.com")})

	// Test checking against both lists
	if !m.IsSuppressed("user1@example.com", []string{"list1", "list2"}) {
		t.Error("user1 should be suppressed by list1")
	}
	if !m.IsSuppressed("user2@example.com", []string{"list1", "list2"}) {
		t.Error("user2 should be suppressed by list2")
	}
	if m.IsSuppressed("user3@example.com", []string{"list1", "list2"}) {
		t.Error("user3 should not be suppressed")
	}

	// Test checking against single list
	if m.IsSuppressed("user1@example.com", []string{"list2"}) {
		t.Error("user1 should not be suppressed by list2 alone")
	}
}

func TestManager_CachingPreventsReload(t *testing.T) {
	m := NewManager()

	hashes := []MD5Hash{MD5HashFromEmail("test@example.com")}

	// Load once
	list1, _ := m.LoadList("cached", "Cached", "test", hashes)

	// Load again - should return same instance
	list2, _ := m.LoadList("cached", "Cached", "test", hashes)

	if list1 != list2 {
		t.Error("LoadList should return cached instance")
	}
}

func TestManager_ThunderingHerdPrevention(t *testing.T) {
	m := NewManager()

	// Create a large list that takes time to load
	hashes := make([]MD5Hash, 10000)
	for i := 0; i < 10000; i++ {
		hashes[i] = generateTestMD5(i)
	}

	var wg sync.WaitGroup
	var loadCount int32
	results := make([]*SuppressionList, 10)
	errors := make([]error, 10)

	// Start 10 concurrent loads of the same list
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			atomic.AddInt32(&loadCount, 1)
			results[idx], errors[idx] = m.LoadList("concurrent", "Concurrent", "test", hashes)
		}(i)
	}

	wg.Wait()

	// All should succeed
	for i, err := range errors {
		if err != nil {
			t.Errorf("Goroutine %d failed: %v", i, err)
		}
	}

	// All should return the same instance
	for i := 1; i < 10; i++ {
		if results[i] != results[0] {
			t.Error("All goroutines should receive the same list instance")
		}
	}
}

func TestManager_FilterEmails(t *testing.T) {
	m := NewManager()

	m.LoadList("filter-test", "Filter Test", "test", []MD5Hash{
		MD5HashFromEmail("blocked1@example.com"),
		MD5HashFromEmail("blocked2@example.com"),
	})

	emails := []string{
		"blocked1@example.com",
		"allowed1@example.com",
		"blocked2@example.com",
		"allowed2@example.com",
	}

	deliverable, suppressed := m.FilterEmails(emails, []string{"filter-test"})

	if suppressed != 2 {
		t.Errorf("Suppressed count = %d, want 2", suppressed)
	}
	if len(deliverable) != 2 {
		t.Errorf("Deliverable count = %d, want 2", len(deliverable))
	}

	// Verify correct emails are in deliverable
	for _, email := range deliverable {
		if strings.HasPrefix(email, "blocked") {
			t.Errorf("Blocked email %s should not be in deliverable", email)
		}
	}
}

func TestManager_UnloadList(t *testing.T) {
	m := NewManager()

	m.LoadList("to-unload", "To Unload", "test", []MD5Hash{MD5HashFromEmail("test@example.com")})

	// Verify it's loaded
	if _, err := m.GetList("to-unload"); err != nil {
		t.Error("List should be loaded")
	}

	// Unload
	m.UnloadList("to-unload")

	// Verify it's unloaded
	if _, err := m.GetList("to-unload"); err != ErrListNotFound {
		t.Error("List should be unloaded")
	}
}

func TestManager_Stats(t *testing.T) {
	m := NewManager()

	m.LoadList("stats1", "Stats 1", "test", []MD5Hash{MD5HashFromEmail("a@test.com")})
	m.LoadList("stats2", "Stats 2", "test", []MD5Hash{
		MD5HashFromEmail("b@test.com"),
		MD5HashFromEmail("c@test.com"),
	})

	// Perform some checks
	m.IsSuppressed("a@test.com", []string{"stats1", "stats2"})
	m.IsSuppressed("d@test.com", []string{"stats1", "stats2"})

	stats := m.Stats()

	if len(stats.Lists) != 2 {
		t.Errorf("Lists count = %d, want 2", len(stats.Lists))
	}
	if stats.TotalRecords != 3 {
		t.Errorf("TotalRecords = %d, want 3", stats.TotalRecords)
	}
	if stats.ChecksTotal != 2 {
		t.Errorf("ChecksTotal = %d, want 2", stats.ChecksTotal)
	}
}

func TestManager_LoadListFromHexStrings(t *testing.T) {
	m := NewManager()

	hexStrings := []string{
		MD5HashFromEmail("hex1@test.com").ToHex(),
		MD5HashFromEmail("hex2@test.com").ToHex(),
		"invalid", // Should be skipped
		"",        // Should be skipped
	}

	list, err := m.LoadListFromHexStrings("hex-list", "Hex List", "test", hexStrings)
	if err != nil {
		t.Fatalf("LoadListFromHexStrings() error = %v", err)
	}

	if list.Count() != 2 {
		t.Errorf("Count = %d, want 2", list.Count())
	}
}

func TestManager_LoadListFromReader(t *testing.T) {
	m := NewManager()

	input := strings.Join([]string{
		MD5HashFromEmail("reader1@test.com").ToHex(),
		MD5HashFromEmail("reader2@test.com").ToHex(),
		"# comment line",
		"  ",
		"reader3@test.com", // Email format - should be hashed
	}, "\n")

	list, err := m.LoadListFromReader("reader-list", "Reader List", "test", strings.NewReader(input))
	if err != nil {
		t.Fatalf("LoadListFromReader() error = %v", err)
	}

	if list.Count() != 3 {
		t.Errorf("Count = %d, want 3", list.Count())
	}

	if !list.ContainsEmail("reader3@test.com") {
		t.Error("Should contain reader3@test.com")
	}
}

func TestManager_GetListNotFound(t *testing.T) {
	m := NewManager()

	_, err := m.GetList("nonexistent")
	if err != ErrListNotFound {
		t.Errorf("Expected ErrListNotFound, got %v", err)
	}
}

func TestManager_IsSuppressedWithMissingList(t *testing.T) {
	m := NewManager()

	// Should not panic, just return false
	if m.IsSuppressed("test@test.com", []string{"nonexistent"}) {
		t.Error("Should return false for nonexistent list")
	}
}

// =============================================================================
// BINARY SEARCH TESTS
// =============================================================================

func TestBinarySearch(t *testing.T) {
	hashes := []MD5Hash{
		{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1},
		{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 3},
		{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 5},
		{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 7},
	}

	// Test finding existing elements
	if !binarySearch(hashes, hashes[0]) {
		t.Error("Should find first element")
	}
	if !binarySearch(hashes, hashes[3]) {
		t.Error("Should find last element")
	}
	if !binarySearch(hashes, hashes[1]) {
		t.Error("Should find middle element")
	}

	// Test not finding missing elements
	notFound := MD5Hash{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2}
	if binarySearch(hashes, notFound) {
		t.Error("Should not find missing element")
	}

	// Test empty slice
	if binarySearch([]MD5Hash{}, hashes[0]) {
		t.Error("Should not find in empty slice")
	}
}

func TestDeduplicateAndSort(t *testing.T) {
	hashes := []MD5Hash{
		{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 5},
		{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1},
		{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 5}, // Duplicate
		{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 3},
		{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}, // Duplicate
	}

	result := deduplicateAndSort(hashes)

	if len(result) != 3 {
		t.Errorf("Length = %d, want 3", len(result))
	}

	// Check sorting
	for i := 1; i < len(result); i++ {
		if result[i].Compare(result[i-1]) <= 0 {
			t.Error("Result should be sorted")
		}
	}
}

// =============================================================================
// BENCHMARK TESTS
// =============================================================================

func BenchmarkBloomFilter_Add(b *testing.B) {
	cfg := DefaultBloomConfig(uint64(b.N))
	bf := NewBloomFilter(cfg)
	hashes := make([]MD5Hash, b.N)
	for i := 0; i < b.N; i++ {
		hashes[i] = generateTestMD5(i)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bf.Add(hashes[i])
	}
}

func BenchmarkBloomFilter_MayContain(b *testing.B) {
	cfg := DefaultBloomConfig(100000)
	bf := NewBloomFilter(cfg)
	for i := 0; i < 100000; i++ {
		bf.Add(generateTestMD5(i))
	}

	hashes := make([]MD5Hash, b.N)
	for i := 0; i < b.N; i++ {
		hashes[i] = generateTestMD5(rand.Intn(200000))
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bf.MayContain(hashes[i])
	}
}

func BenchmarkSuppressionList_Contains(b *testing.B) {
	count := 100000
	hashes := make([]MD5Hash, count)
	for i := 0; i < count; i++ {
		hashes[i] = generateTestMD5(i)
	}

	list, _ := NewSuppressionList("bench", "Bench", "test", hashes)

	// Mix of hits and misses
	testHashes := make([]MD5Hash, b.N)
	for i := 0; i < b.N; i++ {
		testHashes[i] = generateTestMD5(rand.Intn(count * 2))
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		list.Contains(testHashes[i])
	}
}

func BenchmarkManager_IsSuppressed(b *testing.B) {
	m := NewManager()

	// Load multiple lists
	for listNum := 0; listNum < 3; listNum++ {
		hashes := make([]MD5Hash, 50000)
		for i := 0; i < 50000; i++ {
			hashes[i] = generateTestMD5(listNum*100000 + i)
		}
		m.LoadList(fmt.Sprintf("list%d", listNum), "List", "test", hashes)
	}

	listIDs := []string{"list0", "list1", "list2"}
	emails := make([]string, b.N)
	for i := 0; i < b.N; i++ {
		emails[i] = generateTestEmail(rand.Intn(300000))
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.IsSuppressed(emails[i], listIDs)
	}
}

func BenchmarkManager_FilterEmails(b *testing.B) {
	m := NewManager()

	hashes := make([]MD5Hash, 100000)
	for i := 0; i < 100000; i++ {
		hashes[i] = generateTestMD5(i)
	}
	m.LoadList("filter-bench", "Filter Bench", "test", hashes)

	// Create a batch of emails
	emails := make([]string, 1000)
	for i := 0; i < 1000; i++ {
		emails[i] = generateTestEmail(rand.Intn(200000))
	}

	listIDs := []string{"filter-bench"}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.FilterEmails(emails, listIDs)
	}
}

// =============================================================================
// CONCURRENCY TESTS
// =============================================================================

func TestConcurrentAccess(t *testing.T) {
	m := NewManager()

	hashes := make([]MD5Hash, 10000)
	for i := 0; i < 10000; i++ {
		hashes[i] = generateTestMD5(i)
	}
	m.LoadList("concurrent", "Concurrent", "test", hashes)

	var wg sync.WaitGroup
	errors := make(chan error, 100)

	// Multiple readers
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				email := generateTestEmail(rand.Intn(20000))
				m.IsSuppressed(email, []string{"concurrent"})
			}
		}(i)
	}

	// Stats reader
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				m.Stats()
			}
		}()
	}

	wg.Wait()
	close(errors)

	for err := range errors {
		t.Error(err)
	}
}

// =============================================================================
// EDGE CASE TESTS
// =============================================================================

func TestEdgeCase_SingleEntry(t *testing.T) {
	m := NewManager()

	m.LoadList("single", "Single", "test", []MD5Hash{MD5HashFromEmail("only@test.com")})

	if !m.IsSuppressed("only@test.com", []string{"single"}) {
		t.Error("Should find single entry")
	}
	if m.IsSuppressed("other@test.com", []string{"single"}) {
		t.Error("Should not find other entries")
	}
}

func TestEdgeCase_EmptyListIDs(t *testing.T) {
	m := NewManager()

	m.LoadList("test", "Test", "test", []MD5Hash{MD5HashFromEmail("test@test.com")})

	// Empty list IDs should return false
	if m.IsSuppressed("test@test.com", []string{}) {
		t.Error("Should return false for empty list IDs")
	}
	if m.IsSuppressed("test@test.com", nil) {
		t.Error("Should return false for nil list IDs")
	}
}

func TestEdgeCase_SpecialEmails(t *testing.T) {
	m := NewManager()

	specialEmails := []string{
		"test+tag@example.com",
		"test.dot@example.com",
		"TEST@EXAMPLE.COM",
		"  spaces@example.com  ",
		"unicode@ünïcödé.com",
	}

	hashes := make([]MD5Hash, len(specialEmails))
	for i, email := range specialEmails {
		hashes[i] = MD5HashFromEmail(email)
	}

	m.LoadList("special", "Special", "test", hashes)

	for _, email := range specialEmails {
		if !m.IsSuppressed(email, []string{"special"}) {
			t.Errorf("Should find special email: %s", email)
		}
	}
}

func TestEdgeCase_VeryLongEmail(t *testing.T) {
	m := NewManager()

	longEmail := strings.Repeat("a", 200) + "@" + strings.Repeat("b", 100) + ".com"
	m.LoadList("long", "Long", "test", []MD5Hash{MD5HashFromEmail(longEmail)})

	if !m.IsSuppressed(longEmail, []string{"long"}) {
		t.Error("Should find very long email")
	}
}

// =============================================================================
// MEMORY COMPARISON TEST (map vs sorted slice)
// =============================================================================

func TestMemoryComparison(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping memory comparison test in short mode")
	}

	count := 100000

	// Generate hashes
	hashes := make([]MD5Hash, count)
	hexStrings := make([]string, count)
	for i := 0; i < count; i++ {
		hashes[i] = generateTestMD5(i)
		hexStrings[i] = hashes[i].ToHex()
	}

	// Measure map memory (old approach)
	mapSet := make(map[string]bool, count)
	for _, hex := range hexStrings {
		mapSet[hex] = true
	}

	// Measure sorted slice memory (new approach)
	list, _ := NewSuppressionList("mem-test", "Mem Test", "test", hashes)
	stats := list.Stats()

	// Log comparison
	t.Logf("Memory comparison for %d entries:", count)
	t.Logf("  Optimized engine: %.2f KB (Bloom: %.2f KB + Hashes: %.2f KB)",
		float64(stats.TotalMemoryBytes)/1024,
		float64(stats.BloomMemoryBytes)/1024,
		float64(stats.HashArrayMemoryBytes)/1024)
	
	// Estimated map memory (approximately)
	estimatedMapMemory := count * (32 + 16 + 8) // string data + header + bool + bucket overhead
	t.Logf("  Estimated map memory: ~%.2f KB", float64(estimatedMapMemory)/1024)
	
	savings := 100 * (1 - float64(stats.TotalMemoryBytes)/float64(estimatedMapMemory))
	t.Logf("  Estimated savings: ~%.1f%%", savings)
}
