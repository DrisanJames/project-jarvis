package segmentation

import (
	"strings"
	"testing"
)

func TestBuildCountQueryUsesTrackingEventsForEmailSent(t *testing.T) {
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

	if !strings.Contains(query, "mailing_tracking_events") {
		t.Fatalf("expected email_sent query to use mailing_tracking_events, got:\n%s", query)
	}

	if len(args) != 2 {
		t.Fatalf("expected 2 args (org-id + event-type), got %d (%#v)", len(args), args)
	}
	if args[0] != "org-123" {
		t.Fatalf("expected first arg to be org-id, got %#v", args)
	}
	if args[1] != "sent" {
		t.Fatalf("expected second arg to be 'sent', got %#v", args[1])
	}
}

func TestBuildCountQueryUsesTrackingEventsForDomainScopedEmailSent(t *testing.T) {
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

	if !strings.Contains(query, "mailing_tracking_events") {
		t.Fatalf("expected domain-scoped email_sent to use tracking events, got:\n%s", query)
	}
	if !strings.Contains(query, "e.sending_domain ILIKE") {
		t.Fatalf("expected sending-domain filter in query, got:\n%s", query)
	}

	if len(args) != 3 {
		t.Fatalf("expected 3 args, got %d (%#v)", len(args), args)
	}
	if args[0] != "org-123" || args[1] != "%discountblog.com%" || args[2] != "sent" {
		t.Fatalf("unexpected args %#v", args)
	}
}

func TestBuildCountQueryMailedButNotOpened(t *testing.T) {
	qb := NewQueryBuilder()
	qb.SetOrganizationID("org-123")

	group := ConditionGroupBuilder{
		LogicOperator: LogicAnd,
		Conditions: []ConditionBuilder{
			{
				ConditionType: ConditionEvent,
				Operator:      OpEventInLastDays,
				EventName:     "email_sent",
				Value:         "7",
			},
			{
				ConditionType: ConditionEvent,
				Operator:      OpEventNotInLastDays,
				EventName:     "email_opened",
				Value:         "7",
			},
		},
	}

	query, args, err := qb.BuildCountQuery(group, nil)
	if err != nil {
		t.Fatalf("BuildCountQuery() error = %v", err)
	}

	if !strings.Contains(query, "e.event_type = ") {
		t.Fatalf("expected event_type filter, got:\n%s", query)
	}
	if !strings.Contains(query, "EXISTS") {
		t.Fatalf("expected EXISTS clause for sent, got:\n%s", query)
	}
	if !strings.Contains(query, "NOT EXISTS") {
		t.Fatalf("expected NOT EXISTS clause for not-opened, got:\n%s", query)
	}

	if len(args) != 3 {
		t.Fatalf("expected 3 args (org-id + sent + opened), got %d (%#v)", len(args), args)
	}
}
