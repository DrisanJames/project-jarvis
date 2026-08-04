package api

import (
	"strings"
	"testing"
)

// TestBuildSegmentQuery_V2Branch: a {"v2":...} conditions blob must compile
// through the criteria-v2 compiler — NEVER fall through to the legacy array
// parser (whose discarded unmarshal error degrades to an unscoped org-wide
// query, the lake_spec hazard class).
func TestBuildSegmentQuery_V2Branch(t *testing.T) {
	q, args := buildSegmentQuery(`{"v2":{"performance":{"clicked_within_days":30}}}`, nil)
	if !strings.Contains(q, "s.last_click_at >= NOW()") {
		t.Errorf("v2 conditions did not compile through criteria-v2: %s", q)
	}
	if !strings.Contains(q, "s.status IN ('active','confirmed')") {
		t.Errorf("v2 compile missing the index-predicate status filter: %s", q)
	}
	if len(args) != 1 || args[0] != 30 {
		t.Errorf("v2 args: %v", args)
	}
}

// TestBuildSegmentQuery_InvalidV2FailsClosed: a present-but-invalid v2 block
// must return the EMPTY set, not the legacy fallback (which would
// materialize the entire active base).
func TestBuildSegmentQuery_InvalidV2FailsClosed(t *testing.T) {
	for _, raw := range []string{
		`{"v2":{}}`,
		`{"v2":{"lists":["not-a-uuid"]}}`,
		`{"v2":{"refinements":[{"field":"shoe_size","op":"eq","value":"9"}]}}`,
	} {
		q, _ := buildSegmentQuery(raw, nil)
		if q != "SELECT id::text, email FROM mailing_subscribers WHERE FALSE" {
			t.Errorf("invalid v2 %q must fail closed to the empty set, got: %s", raw, q)
		}
	}
}
