package api

import (
	"strings"
	"testing"
)

func TestExcludeNeverClickers_ComposesGuardedClause(t *testing.T) {
	where, args := BuildSegmentWhereClause(nil, []SegmentConditionInput{
		{Group: 0, Field: "sending_domain", Operator: "equals", Value: "em.discountblog.com"},
		{Group: 0, Field: "email_opened", Operator: "in_last_days", Value: "7"},
		{Group: 0, Field: "exclude_never_clickers", Operator: "gte", Value: "15"},
	})
	for _, want := range []string{
		"total_emails_received", "total_clicks", "os_conv.reason = 'converted'",
		"mc_conv.sub1", "NOT (",
	} {
		if !strings.Contains(where, want) {
			t.Fatalf("clause missing %q in:\n%s", want, where)
		}
	}
	found := false
	for _, a := range args {
		if a == "15" {
			found = true
		}
	}
	if !found {
		t.Fatalf("threshold arg 15 not passed: %v", args)
	}
}

func TestExcludeNeverClickers_AbsentByDefault(t *testing.T) {
	where, _ := BuildSegmentWhereClause(nil, []SegmentConditionInput{
		{Group: 0, Field: "sending_domain", Operator: "equals", Value: "em.discountblog.com"},
		{Group: 0, Field: "email_opened", Operator: "in_last_days", Value: "7"},
	})
	if strings.Contains(where, "total_clicks") {
		t.Fatalf("never-clicker clause must not appear without the condition")
	}
}
