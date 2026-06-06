package api

import (
	"strings"
	"testing"
)

// TestBuildSegmentWhereClause_Tags verifies the array-membership predicates
// emitted for the mailing_subscribers.tags TEXT[] column. This is the linchpin
// that lets provenance/vertical cohorts (tags contains_any [vertical:<slug>])
// materialize through the same dynamic-segment pipeline as engagement
// conditions.
func TestBuildSegmentWhereClause_Tags(t *testing.T) {
	tests := []struct {
		name        string
		conditions  []SegmentConditionInput
		wantSQL     []string // substrings that must appear
		wantNotSQL  []string // substrings that must NOT appear
		wantArgVals []string // expected positional array-literal args (in order)
	}{
		{
			name: "contains_any single tag overlap",
			conditions: []SegmentConditionInput{
				{Field: "tags", Operator: "contains_any", Value: "vertical:mortgage"},
			},
			wantSQL:     []string{"tags && $1::text[]"},
			wantNotSQL:  []string{"ILIKE"},
			wantArgVals: []string{`{"vertical:mortgage"}`},
		},
		{
			name: "contains_any multiple tags",
			conditions: []SegmentConditionInput{
				{Field: "tags", Operator: "contains_any", Value: "vertical:mortgage, vertical:finance"},
			},
			wantSQL:     []string{"tags && $1::text[]"},
			wantArgVals: []string{`{"vertical:mortgage","vertical:finance"}`},
		},
		{
			name: "contains_all uses superset operator",
			conditions: []SegmentConditionInput{
				{Field: "tags", Operator: "contains_all", Value: "vertical:mortgage,harvest:keeper"},
			},
			wantSQL:     []string{"tags @> $1::text[]"},
			wantArgVals: []string{`{"vertical:mortgage","harvest:keeper"}`},
		},
		{
			name: "not_contains negates overlap",
			conditions: []SegmentConditionInput{
				{Field: "tags", Operator: "not_contains", Value: "vertical:mortgage"},
			},
			wantSQL:     []string{"NOT (tags && $1::text[])"},
			wantArgVals: []string{`{"vertical:mortgage"}`},
		},
		{
			name: "vertical provenance AND engagement window combine",
			conditions: []SegmentConditionInput{
				{Field: "tags", Operator: "contains_any", Value: "vertical:mortgage"},
				{Field: "email_opened", Operator: "in_last_days", Value: "30"},
			},
			wantSQL: []string{
				"tags && $1::text[]",
				"mailing_tracking_events",
				"event_type = $2",
				"INTERVAL '30 days'",
			},
			wantNotSQL:  []string{"ILIKE"},
			wantArgVals: []string{`{"vertical:mortgage"}`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			where, args := BuildSegmentWhereClause(nil, tt.conditions)

			for _, sub := range tt.wantSQL {
				if !strings.Contains(where, sub) {
					t.Errorf("expected SQL to contain %q\ngot: %s", sub, where)
				}
			}
			for _, sub := range tt.wantNotSQL {
				if strings.Contains(where, sub) {
					t.Errorf("expected SQL NOT to contain %q\ngot: %s", sub, where)
				}
			}

			// Collect array-literal args (strings starting with '{').
			var gotArrArgs []string
			for _, a := range args {
				if s, ok := a.(string); ok && strings.HasPrefix(s, "{") {
					gotArrArgs = append(gotArrArgs, s)
				}
			}
			if len(gotArrArgs) != len(tt.wantArgVals) {
				t.Fatalf("expected %d array args, got %d (%v)", len(tt.wantArgVals), len(gotArrArgs), gotArrArgs)
			}
			for i, want := range tt.wantArgVals {
				if gotArrArgs[i] != want {
					t.Errorf("array arg %d: want %q, got %q", i, want, gotArrArgs[i])
				}
			}
		})
	}
}

// TestBuildSegmentWhereClause_TagsEmptyValue ensures an empty tags value is
// skipped rather than producing a broken predicate.
func TestBuildSegmentWhereClause_TagsEmptyValue(t *testing.T) {
	where, _ := BuildSegmentWhereClause(nil, []SegmentConditionInput{
		{Field: "tags", Operator: "contains_any", Value: "  , "},
	})
	if strings.Contains(where, "tags &&") || strings.Contains(where, "tags @>") {
		t.Errorf("empty tags value should not emit a tags predicate, got: %s", where)
	}
}
