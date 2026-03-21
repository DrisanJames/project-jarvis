package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBloomFilter_MarshalUnmarshal(t *testing.T) {
	bf := NewBloomFilter(10000, 0.001)

	hashes := []string{
		"5d41402abc4b2a76b9719d911017c592",
		"7d793037a0760186574b0282f2f435e7",
		"e99a18c428cb38d5f260853678922e03",
		"d8578edf8458ce06fbc5bb76a58c5ca4",
	}
	for _, h := range hashes {
		bf.AddMD5(h)
	}

	data, err := bf.MarshalBinary()
	require.NoError(t, err)
	require.NotEmpty(t, data)

	restored, err := UnmarshalBloomFilter(data)
	require.NoError(t, err)

	assert.Equal(t, bf.size, restored.size)
	assert.Equal(t, bf.hashCount, restored.hashCount)
	assert.Equal(t, bf.elementCount, restored.elementCount)

	for _, h := range hashes {
		assert.True(t, restored.ContainsMD5(h), "restored filter should contain %s", h)
	}

	assert.False(t, restored.ContainsMD5("0000000000000000000000000000dead"),
		"restored filter should not contain uninserted hash")
}

func TestBloomFilter_UnmarshalInvalidData(t *testing.T) {
	_, err := UnmarshalBloomFilter([]byte{0x00, 0x01})
	assert.Error(t, err, "should fail on too-short data")

	_, err = UnmarshalBloomFilter(nil)
	assert.Error(t, err, "should fail on nil data")
}

func TestBloomFilter_FalsePositiveRate(t *testing.T) {
	bf := NewBloomFilter(100000, 0.01)

	for i := 0; i < 100000; i++ {
		h := md5Hash("test" + string(rune(i)))
		bf.AddMD5(h)
	}

	falsePositives := 0
	checks := 10000
	for i := 0; i < checks; i++ {
		h := md5Hash("nonexistent" + string(rune(i+200000)))
		if bf.ContainsMD5(h) {
			falsePositives++
		}
	}

	fpRate := float64(falsePositives) / float64(checks)
	assert.Less(t, fpRate, 0.05, "false positive rate should be under 5%% (got %.4f)", fpRate)
}
