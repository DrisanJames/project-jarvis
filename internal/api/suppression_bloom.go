// Bloom filter and in-memory suppression matcher.
package api

import (
	"crypto/md5"
	"encoding/hex"
	"strings"
	"sync"
)

// BloomFilter is a space-efficient probabilistic data structure
type BloomFilter struct {
	bitArray     []uint64
	size         uint64
	hashCount    uint
	elementCount uint64
	mu           sync.RWMutex
}

// NewBloomFilter creates a bloom filter optimized for expected elements
func NewBloomFilter(expectedElements uint64, falsePositiveRate float64) *BloomFilter {
	// Calculate optimal size
	m := uint64(float64(expectedElements) * 10) // ~10 bits per element for 1% FP
	if m == 0 {
		m = 1024
	}
	k := uint(7) // Good default for 1% FP rate

	return &BloomFilter{
		bitArray:  make([]uint64, (m+63)/64),
		size:      m,
		hashCount: k,
	}
}

func (bf *BloomFilter) hash(element string, seed uint) uint64 {
	data := []byte(element)
	h := uint64(seed) * 0x9e3779b97f4a7c15
	for _, b := range data {
		h ^= uint64(b)
		h *= 0x517cc1b727220a95
	}
	return h
}

func (bf *BloomFilter) AddMD5(md5Hash string) {
	bf.mu.Lock()
	defer bf.mu.Unlock()
	lower := strings.ToLower(md5Hash)
	for i := uint(0); i < bf.hashCount; i++ {
		hash := bf.hash(lower, i)
		idx := hash % bf.size
		bf.bitArray[idx/64] |= 1 << (idx % 64)
	}
	bf.elementCount++
}

func (bf *BloomFilter) ContainsMD5(md5Hash string) bool {
	bf.mu.RLock()
	defer bf.mu.RUnlock()
	lower := strings.ToLower(md5Hash)
	for i := uint(0); i < bf.hashCount; i++ {
		hash := bf.hash(lower, i)
		idx := hash % bf.size
		if bf.bitArray[idx/64]&(1<<(idx%64)) == 0 {
			return false
		}
	}
	return true
}

func (bf *BloomFilter) Size() uint64 {
	bf.mu.RLock()
	defer bf.mu.RUnlock()
	return bf.elementCount
}

func (bf *BloomFilter) MemoryKB() uint64 {
	return uint64(len(bf.bitArray)) * 8 / 1024
}

// SuppressionMatcher handles efficient suppression matching
type SuppressionMatcher struct {
	filters  map[string]*BloomFilter
	hashSets map[string]map[string]bool
	mu       sync.RWMutex
}

func NewSuppressionMatcher() *SuppressionMatcher {
	return &SuppressionMatcher{
		filters:  make(map[string]*BloomFilter),
		hashSets: make(map[string]map[string]bool),
	}
}

func (sm *SuppressionMatcher) LoadList(listID string, md5Hashes []string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	bf := NewBloomFilter(uint64(len(md5Hashes)), 0.001)
	hashSet := make(map[string]bool, len(md5Hashes))

	for _, hash := range md5Hashes {
		lower := strings.ToLower(hash)
		bf.AddMD5(lower)
		hashSet[lower] = true
	}

	sm.filters[listID] = bf
	sm.hashSets[listID] = hashSet
}

func (sm *SuppressionMatcher) IsSuppressed(email string, listIDs []string) bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	hash := md5.Sum([]byte(strings.ToLower(strings.TrimSpace(email))))
	md5Str := hex.EncodeToString(hash[:])

	for _, listID := range listIDs {
		bf, exists := sm.filters[listID]
		if !exists {
			continue
		}
		if bf.ContainsMD5(md5Str) {
			if hashSet, ok := sm.hashSets[listID]; ok {
				if hashSet[md5Str] {
					return true
				}
			}
		}
	}
	return false
}

func (sm *SuppressionMatcher) IsSuppressedMD5(md5Hash string, listIDs []string) bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	lower := strings.ToLower(md5Hash)
	for _, listID := range listIDs {
		bf, exists := sm.filters[listID]
		if !exists {
			continue
		}
		if bf.ContainsMD5(lower) {
			if hashSet, ok := sm.hashSets[listID]; ok {
				if hashSet[lower] {
					return true
				}
			}
		}
	}
	return false
}

func (sm *SuppressionMatcher) GetStats() map[string]interface{} {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	totalRecords := uint64(0)
	totalMemory := uint64(0)
	lists := []map[string]interface{}{}

	for listID, bf := range sm.filters {
		lists = append(lists, map[string]interface{}{
			"list_id":      listID,
			"record_count": bf.Size(),
			"memory_kb":    bf.MemoryKB(),
		})
		totalRecords += bf.Size()
		totalMemory += bf.MemoryKB()
	}

	return map[string]interface{}{
		"lists":           lists,
		"total_lists":     len(sm.filters),
		"total_records":   totalRecords,
		"total_memory_kb": totalMemory,
	}
}
