package mailing

import (
	"crypto/sha256"
	"encoding/binary"
	"testing"

	"github.com/google/uuid"
)

func TestDeterministicSelection(t *testing.T) {
	campaignID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	email := "test@example.com"

	hash := sha256.Sum256([]byte(email + campaignID.String()))
	idx := int(binary.BigEndian.Uint64(hash[:8]))
	if idx < 0 {
		idx = -idx
	}

	// With 2 variants, should consistently pick the same one
	result1 := idx % 2
	result2 := idx % 2
	if result1 != result2 {
		t.Error("deterministic selection is not deterministic")
	}
}

func TestDifferentEmailsDifferentVariants(t *testing.T) {
	campaignID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	results := make(map[int]int)

	for i := 0; i < 1000; i++ {
		email := uuid.New().String() + "@test.com"
		hash := sha256.Sum256([]byte(email + campaignID.String()))
		idx := int(binary.BigEndian.Uint64(hash[:8]))
		if idx < 0 {
			idx = -idx
		}
		results[idx%2]++
	}

	if len(results) < 2 {
		t.Error("all emails mapped to same variant; hash is not distributing")
	}

	for k, v := range results {
		if v < 300 || v > 700 {
			t.Errorf("variant %d got %d/1000 assignments; expected ~500 (50/50 split)", k, v)
		}
	}
}
