package segmentation

import (
	"strings"
	"testing"
)

func TestBuildCountQueryUsesMessageLogForEmailSentWithoutDomainFilter(t *testing.T) {
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

	if !strings.Contains(query, "FROM mailing_message_log ml") {
		t.Fatalf("expected email_sent query to use mailing_message_log, got %s", query)
	}
	if !strings.Contains(query, "COALESCE(LOWER(ml.email), '') = LOWER(s.email)") {
		t.Fatalf("expected message log email fallback match in query, got %s", query)
	}
	if strings.Contains(query, "mailing_tracking_events") {
		t.Fatalf("expected no direct tracking-event sent query when domain filter is absent, got %s", query)
	}

	if len(args) != 1 {
		t.Fatalf("expected 1 arg, got %d (%#v)", len(args), args)
	}
	if args[0] != "org-123" {
		t.Fatalf("unexpected args %#v", args)
	}
}

func TestBuildCountQueryKeepsTrackingEventsForDomainScopedEmailSent(t *testing.T) {
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
		t.Fatalf("expected domain-scoped email_sent query to stay on tracking events, got %s", query)
	}
	if !strings.Contains(query, "sent_e.campaign_id IS NOT DISTINCT FROM e.campaign_id") {
		t.Fatalf("expected PMTA sent fallback in tracking query, got %s", query)
	}
	if !strings.Contains(query, "e.sending_domain ILIKE") {
		t.Fatalf("expected sending-domain filter in query, got %s", query)
	}

	if len(args) != 5 {
		t.Fatalf("expected 5 args, got %d (%#v)", len(args), args)
	}
	if args[0] != "org-123" || args[1] != "%discountblog.com%" || args[2] != "sent" || args[3] != "delivered" || args[4] != "sent" {
		t.Fatalf("unexpected args %#v", args)
	}
}
