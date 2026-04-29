package api

import (
	"encoding/csv"
	"fmt"
	"io"
	"strings"
)

// canonicalColumns mirrors the schema produced by
// scripts/import/process_eo_cleaned.py and the canonical hooks used by
// .scratch/apr29_load_trugreen_attribits.py. Order matters for clarity
// only — the handler resolves column positions by name, so a CSV with
// the same set of columns in a different order is still accepted.
var canonicalColumns = []string{
	"email", "email_hash", "first_name", "last_name", "isp",
	"eo_domain_group", "tags", "source_detail", "source_metadata", "custom_fields",
}

// canonicalCSVReader is a thin wrapper around encoding/csv.Reader. We
// expose a stable type so callers don't have to re-declare quoting and
// field-handling preferences in multiple places.
type canonicalCSVReader struct {
	r *csv.Reader
}

func newCanonicalCSVReader(r io.Reader) *canonicalCSVReader {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1 // tolerate ragged rows; we validate after read
	cr.LazyQuotes = false
	cr.ReuseRecord = false
	cr.TrimLeadingSpace = false
	return &canonicalCSVReader{r: cr}
}

func (c *canonicalCSVReader) Read() ([]string, error) {
	rec, err := c.r.Read()
	if err == io.EOF {
		return nil, fmt.Errorf("EOF")
	}
	return rec, err
}

// canonicalColIndex maps each required column to its position in the
// supplied header row. Returns an error listing every missing column.
func canonicalColIndex(header []string) (map[string]int, error) {
	idx := make(map[string]int, len(canonicalColumns))
	for i, h := range header {
		idx[strings.TrimSpace(strings.ToLower(h))] = i
	}
	missing := []string{}
	for _, col := range canonicalColumns {
		if _, ok := idx[col]; !ok {
			missing = append(missing, col)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("canonical CSV missing columns: %s", strings.Join(missing, ", "))
	}
	return idx, nil
}
