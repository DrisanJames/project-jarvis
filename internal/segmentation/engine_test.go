package segmentation

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

func TestPreviewSegmentScansPostgresTagsArray(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	engine := NewEngine(db)
	orgID := uuid.New()
	listID := uuid.New()
	subscriberID := uuid.New()
	now := time.Now()

	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM mailing_subscribers s").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	mock.ExpectQuery("SELECT s.id, s.organization_id, s.list_id, s.email, s.first_name, s.last_name,").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "organization_id", "list_id", "email", "first_name", "last_name",
			"status", "engagement_score", "total_opens", "total_clicks", "last_open_at",
			"last_click_at", "optimal_send_hour_utc", "timezone", "subscribed_at",
			"custom_fields", "tags",
		}).AddRow(
			subscriberID, orgID, listID, "reader@example.com", "Reader", "Example",
			"confirmed", 42.5, 3, 1, now, now, 9, "UTC", now,
			json.RawMessage(`{"tier":"vip"}`), []byte("{promo,discount}"),
		))

	preview, err := engine.PreviewSegment(context.Background(), orgID, nil, ConditionGroupBuilder{
		LogicOperator: LogicAnd,
	}, nil, 10)
	if err != nil {
		t.Fatalf("PreviewSegment() error = %v", err)
	}
	if preview == nil {
		t.Fatal("PreviewSegment() returned nil preview")
	}
	if preview.EstimatedCount != 1 {
		t.Fatalf("expected estimated count 1, got %d", preview.EstimatedCount)
	}
	if len(preview.SampleSubscribers) != 1 {
		t.Fatalf("expected 1 sample subscriber, got %d", len(preview.SampleSubscribers))
	}
	if preview.SampleSubscribers[0].Email != "reader@example.com" {
		t.Fatalf("unexpected sample subscriber %#v", preview.SampleSubscribers[0])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations were not met: %v", err)
	}
}
