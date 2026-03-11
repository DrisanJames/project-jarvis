package engine

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPersistToDB_DeferralTypes verifies that PMTA transient records (type "t"
// and "tq") are persisted to mailing_tracking_events with event_type "deferred".
// Before this fix, these records hit the default branch and were silently
// discarded, making deferrals invisible in the analytics layer.
func TestPersistToDB_DeferralTypes(t *testing.T) {
	tests := []struct {
		name          string
		recType       string
		wantEventType string
		wantPersisted bool
	}{
		{"delivery is persisted", "d", "delivered", true},
		{"bounce is persisted", "b", "bounced", true},
		{"complaint is persisted", "f", "complained", true},
		{"transient is persisted as deferred", "t", "deferred", true},
		{"transient-queue is persisted as deferred", "tq", "deferred", true},
		{"unknown type is skipped", "x", "", false},
	}

	campaignUUID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	orgUUID := "11111111-2222-3333-4444-555555555555"
	subUUID := "66666666-7777-8888-9999-aaaaaaaaaaaa"

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()

			ing := &Ingestor{db: db}

			rec := AccountingRecord{
				Type:      tc.recType,
				Recipient: "user@comcast.net",
				Sender:    "sender@mail.projectjarvis.io",
				JobID:     campaignUUID,
				SourceIP:  "15.204.22.176",
				VMTA:      "mta1",
			}

			if !tc.wantPersisted {
				// No DB calls expected — the function returns early
				ing.persistToDB(rec, ISP("comcast"))
				assert.NoError(t, mock.ExpectationsWereMet())
				return
			}

			// Campaign ID is a valid UUID, so it skips the message_log lookup.
			// Expect the subscriber + org lookup.
			mock.ExpectQuery("SELECT s.id::text, c.organization_id::text").
				WithArgs(sqlmock.AnyArg(), "user@comcast.net").
				WillReturnRows(
					sqlmock.NewRows([]string{"id", "organization_id"}).
						AddRow(subUUID, orgUUID),
				)

			// Expect the INSERT into mailing_tracking_events
			mock.ExpectExec("INSERT INTO mailing_tracking_events").
				WithArgs(
					sqlmock.AnyArg(), // event ID
					sqlmock.AnyArg(), // org ID
					sqlmock.AnyArg(), // campaign ID
					sqlmock.AnyArg(), // subscriber ID
					tc.wantEventType,
					sqlmock.AnyArg(), // bounce_type
					sqlmock.AnyArg(), // bounce_reason (dsn_status)
					sqlmock.AnyArg(), // sending_domain
					sqlmock.AnyArg(), // sending_ip
					sqlmock.AnyArg(), // recipient_domain
				).
				WillReturnResult(sqlmock.NewResult(0, 1))

			// Expect campaign counter update (varies by event type)
			switch tc.wantEventType {
			case "delivered":
				mock.ExpectExec("UPDATE mailing_campaigns SET delivered_count").
					WithArgs(sqlmock.AnyArg()).
					WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectExec("UPDATE mailing_ip_addresses SET total_delivered").
					WithArgs(sqlmock.AnyArg()).
					WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectExec("INSERT INTO mailing_inbox_profiles").
					WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg()).
					WillReturnResult(sqlmock.NewResult(0, 1))
			case "bounced":
				mock.ExpectExec("UPDATE mailing_campaigns SET bounce_count").
					WithArgs(sqlmock.AnyArg()).
					WillReturnResult(sqlmock.NewResult(0, 1))
				// soft_bounce_count because default BounceCat is empty (not a hard category)
				mock.ExpectExec("UPDATE mailing_campaigns SET soft_bounce_count").
					WithArgs(sqlmock.AnyArg()).
					WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectExec("UPDATE mailing_ip_addresses SET total_bounced").
					WithArgs(sqlmock.AnyArg()).
					WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectExec("INSERT INTO mailing_inbox_profiles").
					WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg()).
					WillReturnResult(sqlmock.NewResult(0, 1))
			case "complained":
				mock.ExpectExec("UPDATE mailing_campaigns SET complaint_count").
					WithArgs(sqlmock.AnyArg()).
					WillReturnResult(sqlmock.NewResult(0, 1))
			case "deferred":
				// No campaign counter update expected.
			}

			ing.persistToDB(rec, ISP("comcast"))

			assert.NoError(t, mock.ExpectationsWereMet(),
				"all expected DB calls must have been made for type=%q", tc.recType)
		})
	}
}

// TestRouteToCampaignTracker_DeferralNotSkipped verifies that transient PMTA
// records are now forwarded to the CampaignEventTracker with event_type
// "deferred", rather than being silently dropped.
func TestRouteToCampaignTracker_DeferralNotSkipped(t *testing.T) {
	tracker := &mockTracker{}
	ing := &Ingestor{
		tracker: tracker,
	}

	rec := AccountingRecord{
		Type:      "t",
		Recipient: "user@gmail.com",
		JobID:     "campaign-123",
		SourceIP:  "15.204.22.176",
		BounceCat: "quota-issues",
		DSNStatus: "4.2.1",
		DSNDiag:   "Mailbox full",
	}

	ing.routeToCampaignTracker(rec, ISP("gmail"))

	require.Len(t, tracker.events, 1, "deferral should produce one campaign event")
	assert.Equal(t, "deferred", tracker.events[0].EventType)
	assert.Equal(t, "campaign-123", tracker.events[0].CampaignID)
	assert.Equal(t, "user@gmail.com", tracker.events[0].Recipient)
}

// TestRouteToCampaignTracker_UnknownTypeSkipped verifies unknown PMTA record
// types do not produce campaign events.
func TestRouteToCampaignTracker_UnknownTypeSkipped(t *testing.T) {
	tracker := &mockTracker{}
	ing := &Ingestor{tracker: tracker}

	rec := AccountingRecord{Type: "x", Recipient: "user@gmail.com", JobID: "camp-1"}
	ing.routeToCampaignTracker(rec, ISP("gmail"))

	assert.Empty(t, tracker.events, "unknown type should not produce events")
}

func TestPersistToDB_HardBounceUpdatesHardCount(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	ing := &Ingestor{db: db}
	campaignUUID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	orgUUID := "11111111-2222-3333-4444-555555555555"
	subUUID := "66666666-7777-8888-9999-aaaaaaaaaaaa"

	rec := AccountingRecord{
		Type:      "b",
		Recipient: "user@gmail.com",
		Sender:    "hello@em.discountblog.com",
		JobID:     campaignUUID,
		SourceIP:  "15.204.22.176",
		BounceCat: "bad-mailbox",
		DSNStatus: "5.1.1",
	}

	mock.ExpectQuery("SELECT s.id::text, c.organization_id::text").
		WithArgs(sqlmock.AnyArg(), "user@gmail.com").
		WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id"}).AddRow(subUUID, orgUUID))
	mock.ExpectExec("INSERT INTO mailing_tracking_events").
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			"bounced", "bad-mailbox", "5.1.1", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE mailing_campaigns SET bounce_count").
		WithArgs(sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE mailing_campaigns SET hard_bounce_count").
		WithArgs(sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE mailing_ip_addresses SET total_bounced").
		WithArgs(sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO mailing_inbox_profiles").
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))

	ing.persistToDB(rec, ISP("gmail"))
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestIsHardBounceCategory(t *testing.T) {
	hard := []string{"bad-mailbox", "bad-domain", "inactive-mailbox", "no-answer-from-host", "routing-errors"}
	soft := []string{"quota-issues", "spam-related", "policy-related", "protocol-errors", "content-related", "other", ""}

	for _, cat := range hard {
		assert.True(t, isHardBounceCategory(cat), "category %q should be hard", cat)
	}
	for _, cat := range soft {
		assert.False(t, isHardBounceCategory(cat), "category %q should be soft", cat)
	}
}

func TestRouteToGlobalSuppression_HardBounce(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	hub := NewGlobalSuppressionHub(db, "org1", "")
	ing := &Ingestor{globalHub: hub}

	rec := AccountingRecord{
		Type:      "b",
		Recipient: "badbounce@example.com",
		BounceCat: "bad-mailbox",
		DSNStatus: "5.1.1",
		SourceIP:  "15.204.22.176",
		JobID:     "campaign-1",
	}

	mock.ExpectExec("INSERT INTO mailing_global_suppressions").
		WillReturnResult(sqlmock.NewResult(0, 1))

	ing.routeToGlobalSuppression(rec, ISP("other"))
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestRouteToGlobalSuppression_SoftBounceNotSuppressed(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	hub := NewGlobalSuppressionHub(db, "org1", "")
	ing := &Ingestor{globalHub: hub}

	rec := AccountingRecord{
		Type:      "b",
		Recipient: "soft@example.com",
		BounceCat: "content-related",
		DSNStatus: "4.3.1",
	}

	ing.routeToGlobalSuppression(rec, ISP("other"))
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestRouteToGlobalSuppression_FBLSuppresses(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	hub := NewGlobalSuppressionHub(db, "org1", "")
	ing := &Ingestor{globalHub: hub}

	rec := AccountingRecord{
		Type:         "f",
		Recipient:    "complaint@example.com",
		FeedbackType: "abuse",
		JobID:        "campaign-2",
	}

	mock.ExpectExec("INSERT INTO mailing_global_suppressions").
		WillReturnResult(sqlmock.NewResult(0, 1))

	ing.routeToGlobalSuppression(rec, ISP("gmail"))
	assert.NoError(t, mock.ExpectationsWereMet())
}

// mockTracker records CampaignEvents for assertion in tests.
type mockTracker struct {
	events []CampaignEvent
}

func (m *mockTracker) RecordEvent(e CampaignEvent) {
	m.events = append(m.events, e)
}
