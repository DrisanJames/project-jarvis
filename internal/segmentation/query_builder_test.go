package segmentation

import (
	"strings"
	"testing"
)

func TestBuildCountQueryUsesDeliveredForEmailSentWithoutDomainFilter(t *testing.T) {
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

	if !strings.Contains(query, "FROM mailing_tracking_events e") {
		t.Fatalf("expected email_sent query to use tracking events, got %s", query)
	}
	if !strings.Contains(query, "e.event_type =") {
		t.Fatalf("expected tracking event filter in query, got %s", query)
	}
	if strings.Contains(query, "mailing_message_log") {
		t.Fatalf("expected no message_log sent query once email_sent maps to delivered, got %s", query)
	}

	if len(args) != 2 {
		t.Fatalf("expected 2 args, got %d (%#v)", len(args), args)
	}
	if args[0] != "org-123" || args[1] != "delivered" {
		t.Fatalf("unexpected args %#v", args)
	}
}

func TestBuildCountQueryUsesDeliveredForDomainScopedEmailSent(t *testing.T) {
	qb := NewQueryBuilder()
	qb.SetOrganizationID("org-123")
	qb.SetTrackingEmailMatchEnabled(true)

	group := ConditionGroupBuilder{
		LogicOperator: LogicAnd,
		Conditions: []ConditionBuilder{
			{
				ConditionType:       ConditionEvent,
				Operator:            OpEquals,
				EventName:           "email_sent",
				EventTimeWindowDays: 7,
				EventSendingDomain:  "discountblog.com",
			},
		},
	}

	query, args, err := qb.BuildCountQuery(group, nil)
	if err != nil {
		t.Fatalf("BuildCountQuery() error = %v", err)
	}

	if !strings.Contains(query, "FROM mailing_tracking_events e") {
		t.Fatalf("expected domain-scoped email_sent query to use tracking events, got %s", query)
	}
	if strings.Contains(query, "sent_e.campaign_id IS NOT DISTINCT FROM e.campaign_id") || strings.Contains(query, "SELECT 1 FROM mailing_tracking_events sent_e") {
		t.Fatalf("expected no sent fallback once email_sent maps directly to delivered, got %s", query)
	}
	if !strings.Contains(query, "e.sending_domain ILIKE") {
		t.Fatalf("expected sending-domain filter in query, got %s", query)
	}

	if len(args) != 3 {
		t.Fatalf("expected 3 args, got %d (%#v)", len(args), args)
	}
	if args[0] != "org-123" || args[1] != "%discountblog.com%" || args[2] != "delivered" {
		t.Fatalf("unexpected args %#v", args)
	}
}
