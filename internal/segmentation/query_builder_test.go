package segmentation

import (
	"strings"
	"testing"
)

func TestBuildCountQueryTreatsDeliveredAsFallbackForEmailSent(t *testing.T) {
	qb := NewQueryBuilder()
	qb.SetOrganizationID("org-123")

	group := ConditionGroupBuilder{
		LogicOperator: LogicAnd,
		Conditions: []ConditionBuilder{
			{
				ConditionType:       ConditionEvent,
				Operator:            OpEquals,
				EventName:           "email_sent",
				EventTimeWindowDays: 7,
			},
		},
	}

	query, args, err := qb.BuildCountQuery(group, nil)
	if err != nil {
		t.Fatalf("BuildCountQuery() error = %v", err)
	}

	if !strings.Contains(query, "sent_e.campaign_id IS NOT DISTINCT FROM e.campaign_id") {
		t.Fatalf("expected PMTA sent fallback in query, got %s", query)
	}
	if !strings.Contains(query, "NOT EXISTS") {
		t.Fatalf("expected NOT EXISTS fallback guard in query, got %s", query)
	}

	if len(args) != 4 {
		t.Fatalf("expected 4 args, got %d (%#v)", len(args), args)
	}
	if args[0] != "org-123" || args[1] != "sent" || args[2] != "delivered" || args[3] != "sent" {
		t.Fatalf("unexpected args %#v", args)
	}
}
