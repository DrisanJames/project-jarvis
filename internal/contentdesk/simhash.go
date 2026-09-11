package contentdesk

import (
	"hash/fnv"
	"math/bits"
	"strings"
	"unicode"
)

// Simhash is a 64-bit simhash over word 3-shingles. HEURISTIC: a small
// Hamming distance means "probably near-duplicate", nothing more — the check
// that uses it is labelled heuristic and surfaced for review.
func Simhash(text string) uint64 {
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	if len(words) == 0 {
		return 0
	}
	var v [64]int
	add := func(feature string) {
		h := fnv.New64a()
		h.Write([]byte(feature))
		x := h.Sum64()
		for i := 0; i < 64; i++ {
			if x&(1<<uint(i)) != 0 {
				v[i]++
			} else {
				v[i]--
			}
		}
	}
	if len(words) < 3 {
		add(strings.Join(words, " "))
	} else {
		for i := 0; i+3 <= len(words); i++ {
			add(words[i] + " " + words[i+1] + " " + words[i+2])
		}
	}
	var out uint64
	for i := 0; i < 64; i++ {
		if v[i] > 0 {
			out |= 1 << uint(i)
		}
	}
	return out
}

// Hamming is the bit distance between two simhashes.
func Hamming(a, b uint64) int { return bits.OnesCount64(a ^ b) }

// PackageBodyText is the text simhash compares (title + blocks).
func PackageBodyText(p Package) string {
	parts := []string{p.Title}
	for _, b := range p.Blocks {
		parts = append(parts, BlockParagraph(b))
	}
	return strings.Join(parts, " ")
}
