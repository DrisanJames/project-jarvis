package segmentation

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

func TestGetSegmentConditionsPrefersSerializedConditions(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	store := NewStore(db)
	segmentID := uuid.New()
	conditionsJSON := `{
		"logic_operator":"AND",
		"is_negated":false,
		"conditions":[
			{
				"condition_type":"event",
				"field":"",
				"operator":"equals",
				"value":"1",
				"event_name":"email_sent",
				"event_time_window_days":7,
				"event_sending_domain":"discountblog.com"
			}
		],
		"groups":[
			{
				"logic_operator":"AND",
				"is_negated":true,
				"conditions":[
					{
						"condition_type":"event",
						"field":"",
						"operator":"equals",
						"value":"1",
						"event_name":"email_clicked",
						"event_time_window_days":7
					}
				],
				"groups":[]
			}
		]
	}`

	mock.ExpectQuery("SELECT conditions\\s+FROM mailing_segments\\s+WHERE id = \\$1").
		WithArgs(segmentID).
		WillReturnRows(sqlmock.NewRows([]string{"conditions"}).AddRow([]byte(conditionsJSON)))

	conditions, err := store.GetSegmentConditions(context.Background(), segmentID)
	if err != nil {
		t.Fatalf("GetSegmentConditions() error = %v", err)
	}
	if conditions == nil {
		t.Fatal("GetSegmentConditions() returned nil conditions")
	}
	if conditions.LogicOperator != LogicAnd {
		t.Fatalf("expected root logic operator %q, got %q", LogicAnd, conditions.LogicOperator)
	}
	if len(conditions.Conditions) != 1 {
		t.Fatalf("expected 1 root condition, got %d", len(conditions.Conditions))
	}
	if got := conditions.Conditions[0].EventSendingDomain; got != "discountblog.com" {
		t.Fatalf("expected event_sending_domain to be preserved, got %q", got)
	}
	if len(conditions.Groups) != 1 || len(conditions.Groups[0].Conditions) != 1 {
		t.Fatalf("expected nested negated click group to be preserved, got %#v", conditions.Groups)
	}
	if conditions.Groups[0].Conditions[0].EventName != "email_clicked" {
		t.Fatalf("expected nested event_name email_clicked, got %q", conditions.Groups[0].Conditions[0].EventName)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations were not met: %v", err)
	}
}
