package contentdesk

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// CanonicalJSON renders v as JSON with object keys sorted at every depth and
// numbers preserved verbatim, so two semantically equal values hash equal
// regardless of struct field order or map iteration order.
func CanonicalJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var generic any
	if err := dec.Decode(&generic); err != nil {
		return nil, err
	}
	// encoding/json sorts map[string]any keys; json.Number marshals verbatim.
	return json.Marshal(generic)
}

// HashJSON is sha256 hex over CanonicalJSON(v).
func HashJSON(v any) (string, error) {
	b, err := CanonicalJSON(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// SortClaimRefs orders refs deterministically (claim_id, version, block_id,
// sentence_idx) — the hash must not depend on the order the model emitted.
func SortClaimRefs(refs []ClaimRef) []ClaimRef {
	out := append([]ClaimRef(nil), refs...)
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.ClaimID != b.ClaimID {
			return a.ClaimID < b.ClaimID
		}
		if a.Version != b.Version {
			return a.Version < b.Version
		}
		if a.BlockID != b.BlockID {
			return a.BlockID < b.BlockID
		}
		return a.SentenceIdx < b.SentenceIdx
	})
	return out
}

// RevisionHash = sha256 over the canonical JSON of the package plus the
// sorted claim refs. Approval binds to this value.
func RevisionHash(pkg Package, refs []ClaimRef) (string, error) {
	return HashJSON(map[string]any{
		"package":    pkg,
		"claim_refs": SortClaimRefs(refs),
	})
}

// ManifestHash is sha256 over the canonical JSON of the manifest sorted by
// slug then article id.
func ManifestHash(entries []ManifestEntry) (string, error) {
	sorted := append([]ManifestEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Slug != sorted[j].Slug {
			return sorted[i].Slug < sorted[j].Slug
		}
		return sorted[i].ArticleID < sorted[j].ArticleID
	})
	return HashJSON(sorted)
}

func itoa(i int) string { return strconv.Itoa(i) }

func splitComma(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}
