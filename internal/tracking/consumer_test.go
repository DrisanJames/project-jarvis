package tracking

// Tests for the SQS consumer's open/click write path.
//
// The consumer is the dominant production write path for 'opened' and
// 'clicked' rows. Before this fix, its INSERT statements omitted
// recipient_domain, which forced every per-ISP report to reconstruct
// the bucket via a join-back to mailing_subscribers at query time —
// and silently folded the ~28% of rows whose subscriber_id no longer
// matched into 'other'.
//
// These tests lock in two invariants:
//
//  1. Every 'opened' / 'clicked' INSERT includes recipient_domain and
//     references mailing_subscribers via LEFT JOIN.
//  2. The LEFT JOIN is by-design: when the subscriber row is missing,
//     the row still writes (recipient_domain stays NULL) and downstream
//     reporting surfaces it as `unresolved_subscriber`. The INSERT must
//     not return an error in that case.
//
// We deliberately do NOT also write a denormalized `email` column: the
// schema does not have one, and subscriber_id is the canonical FK.

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

const (
	testOrgID        = "11111111-1111-1111-1111-111111111111"
	testCampaignID   = "22222222-2222-2222-2222-222222222222"
	testSubscriberID = "33333333-3333-3333-3333-333333333333"
	testEmailID      = "44444444-4444-4444-4444-444444444444"
)

// newConsumer builds a Consumer wrapping a sqlmock DB. The SQS client
// stays nil because these tests exercise processOpen / processClick
// directly, not the poll loop.
func newConsumer(t *testing.T) (*Consumer, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return &Consumer{db: db}, mock
}

// openInsertRegex asserts the open INSERT shape the SQS consumer
// MUST produce post-fix:
//   - column list contains recipient_domain
//   - SELECT contains LOWER(SPLIT_PART(s.email, '@', 2))
//   - FROM clause includes LEFT JOIN mailing_subscribers s ON s.id = $4::uuid
//
// The regex uses (?is) so . matches newlines and matching is case-
// insensitive. Any regression that drops one of these substrings
// will fail the test before the row is ever written in production.
var openInsertRegex = regexp.MustCompile(
	`(?is)INSERT\s+INTO\s+mailing_tracking_events\s*\(.*recipient_domain.*\)` +
		`.*SELECT.*'opened'.*LOWER\(\s*SPLIT_PART\(\s*s\.email\s*,\s*'@'\s*,\s*2\s*\)\s*\).*` +
		`FROM\s+mailing_campaigns\s+c\s+LEFT\s+JOIN\s+mailing_subscribers\s+s\s+ON\s+s\.id\s*=\s*\$4::uuid`,
)

var clickInsertRegex = regexp.MustCompile(
	`(?is)INSERT\s+INTO\s+mailing_tracking_events\s*\(.*recipient_domain.*\)` +
		`.*SELECT.*'clicked'.*LOWER\(\s*SPLIT_PART\(\s*s\.email\s*,\s*'@'\s*,\s*2\s*\)\s*\).*` +
		`FROM\s+mailing_campaigns\s+c\s+LEFT\s+JOIN\s+mailing_subscribers\s+s\s+ON\s+s\.id\s*=\s*\$4::uuid`,
)

func TestProcessOpen_InsertsRecipientDomain(t *testing.T) {
	c, mock := newConsumer(t)
	now := time.Now().UTC()

	// 1. Idempotency check: row does NOT yet exist.
	mock.ExpectQuery(`(?is)SELECT\s+EXISTS.*event_type\s*=\s*'opened'`).
		WithArgs(uuid.MustParse(testEmailID), now).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))

	// 1b. Cross-source dedupe gate: this is the first time we've seen
	// (campaign_id, subscriber_id), so the INSERT inserts and RETURNING
	// yields true. All downstream side effects then run.
	mock.ExpectQuery(`(?is)INSERT\s+INTO\s+mailing_open_dedupe`).
		WithArgs(
			uuid.MustParse(testCampaignID),
			uuid.MustParse(testSubscriberID),
			uuid.MustParse(testEmailID),
			now,
		).
		WillReturnRows(sqlmock.NewRows([]string{"true"}).AddRow(true))

	// 2. Subscriber email lookup (used downstream for inbox_profiles UPDATE).
	mock.ExpectQuery(`(?is)SELECT\s+email\s+FROM\s+mailing_subscribers\s+WHERE\s+id\s*=\s*\$1`).
		WithArgs(uuid.MustParse(testSubscriberID)).
		WillReturnRows(sqlmock.NewRows([]string{"email"}).AddRow("foo@gmail.com"))

	// 3. MPP detection: no prior 'sent' event in window — return ErrNoRows.
	mock.ExpectQuery(`(?is)SELECT\s+event_at.*event_type\s*=\s*'sent'`).
		WithArgs(uuid.MustParse(testSubscriberID), uuid.MustParse(testCampaignID)).
		WillReturnRows(sqlmock.NewRows([]string{"event_at"}))

	// 4. The INSERT — must match the new shape with recipient_domain,
	// email, and the LEFT JOIN to mailing_subscribers.
	mock.ExpectExec(openInsertRegex.String()).
		WithArgs(
			uuid.MustParse(testEmailID),
			uuid.MustParse(testOrgID),
			uuid.MustParse(testCampaignID),
			uuid.MustParse(testSubscriberID),
			now,
			"1.2.3.4",
			"Mozilla/5.0 (X11; Linux x86_64)",
			"desktop",
			false, // isMachineOpen
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// 5. Aggregate UPDATEs that follow a successful INSERT (open_count,
	// total_opens, inbox_profiles, engagement score recompute). We
	// allow them to be hit but don't assert on their exact shape — that
	// is owned by their own tests if they exist. AnyArg loosens
	// argument matching for the engagement recompute in particular,
	// which performs a SELECT ... FROM mailing_subscribers first.
	mock.ExpectExec(`UPDATE\s+mailing_campaigns`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE\s+mailing_subscribers\s+SET\s+total_opens`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE\s+mailing_inbox_profiles`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`(?is)SELECT\s+COALESCE\(total_opens,0\)`).
		WithArgs(uuid.MustParse(testSubscriberID)).
		WillReturnRows(sqlmock.NewRows([]string{"total_opens", "total_clicks", "total_emails", "last_open_at"}).
			AddRow(10, 1, 50, nil))
	mock.ExpectExec(`UPDATE\s+mailing_subscribers\s+SET\s+engagement_score`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := c.processOpen(context.Background(), TrackingEvent{
		EventType:    EventOpen,
		OrgID:        testOrgID,
		CampaignID:   testCampaignID,
		SubscriberID: testSubscriberID,
		EmailID:      testEmailID,
		IPAddress:    "1.2.3.4",
		UserAgent:    "Mozilla/5.0 (X11; Linux x86_64)",
		Timestamp:    now,
	})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestProcessOpen_InsertSucceedsWhenSubscriberMissing(t *testing.T) {
	// When mailing_subscribers has no row matching subscriberID (deleted
	// or migrated), the LEFT JOIN gives s.email = NULL, which becomes
	// recipient_domain = NULL on the inserted row. The INSERT itself
	// must still succeed — this is the population P3 surfaces as
	// `unresolved_subscriber`.
	c, mock := newConsumer(t)
	now := time.Now().UTC()

	mock.ExpectQuery(`(?is)SELECT\s+EXISTS.*event_type\s*=\s*'opened'`).
		WithArgs(uuid.MustParse(testEmailID), now).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))

	// Cross-source dedupe gate — first open, RETURNING yields a row.
	mock.ExpectQuery(`(?is)INSERT\s+INTO\s+mailing_open_dedupe`).
		WithArgs(
			uuid.MustParse(testCampaignID),
			uuid.MustParse(testSubscriberID),
			uuid.MustParse(testEmailID),
			now,
		).
		WillReturnRows(sqlmock.NewRows([]string{"true"}).AddRow(true))

	// Subscriber email lookup returns no rows — Scan leaves email = "".
	mock.ExpectQuery(`(?is)SELECT\s+email\s+FROM\s+mailing_subscribers`).
		WithArgs(uuid.MustParse(testSubscriberID)).
		WillReturnRows(sqlmock.NewRows([]string{"email"}))

	mock.ExpectQuery(`(?is)SELECT\s+event_at.*event_type\s*=\s*'sent'`).
		WithArgs(uuid.MustParse(testSubscriberID), uuid.MustParse(testCampaignID)).
		WillReturnRows(sqlmock.NewRows([]string{"event_at"}))

	// INSERT still uses the same shape — sqlmock matches the regex even
	// though the underlying LEFT JOIN would yield NULL for s.email at
	// real query time.
	mock.ExpectExec(openInsertRegex.String()).
		WithArgs(
			uuid.MustParse(testEmailID),
			uuid.MustParse(testOrgID),
			uuid.MustParse(testCampaignID),
			uuid.MustParse(testSubscriberID),
			now,
			"5.6.7.8",
			"GoogleImageProxy",
			"desktop",
			false,
		).
		// RowsAffected = 0 means no aggregate UPDATEs follow — the
		// test exits the function early. This keeps the test focused
		// on the INSERT shape itself.
		WillReturnResult(sqlmock.NewResult(0, 0))

	err := c.processOpen(context.Background(), TrackingEvent{
		EventType:    EventOpen,
		OrgID:        testOrgID,
		CampaignID:   testCampaignID,
		SubscriberID: testSubscriberID,
		EmailID:      testEmailID,
		IPAddress:    "5.6.7.8",
		UserAgent:    "GoogleImageProxy",
		Timestamp:    now,
	})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestProcessOpen_DuplicateDedupe_SkipsSideEffects asserts that when the
// dedupe gate finds an existing (campaign_id, subscriber_id) row, the
// consumer returns BEFORE running the subscriber lookup, MPP detection,
// tracking_events INSERT, or any of the aggregate UPDATEs. Strict sqlmock
// fails the test if any of those queries fire.
func TestProcessOpen_DuplicateDedupe_SkipsSideEffects(t *testing.T) {
	c, mock := newConsumer(t)
	now := time.Now().UTC()

	// Fast-path EXISTS — row not present, so we proceed to the dedupe gate.
	mock.ExpectQuery(`(?is)SELECT\s+EXISTS.*event_type\s*=\s*'opened'`).
		WithArgs(uuid.MustParse(testEmailID), now).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))

	// Dedupe gate hits the existing row → RETURNING yields no rows →
	// Scan returns sql.ErrNoRows → consumer returns nil immediately.
	mock.ExpectQuery(`(?is)INSERT\s+INTO\s+mailing_open_dedupe`).
		WithArgs(
			uuid.MustParse(testCampaignID),
			uuid.MustParse(testSubscriberID),
			uuid.MustParse(testEmailID),
			now,
		).
		WillReturnRows(sqlmock.NewRows([]string{"true"})) // empty -> ErrNoRows

	err := c.processOpen(context.Background(), TrackingEvent{
		EventType:    EventOpen,
		OrgID:        testOrgID,
		CampaignID:   testCampaignID,
		SubscriberID: testSubscriberID,
		EmailID:      testEmailID,
		Timestamp:    now,
	})
	require.NoError(t, err)
	// CRITICAL: no other queries fired — the dedupe gate short-circuited
	// the entire side-effect path.
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestProcessOpen_DedupeError_FailsClosed asserts the fail-closed posture:
// any error from the dedupe gate skips all side effects. Failing open
// would let dual-pixel renders silently double-count counters whenever
// the gate is flaky.
func TestProcessOpen_DedupeError_FailsClosed(t *testing.T) {
	c, mock := newConsumer(t)
	now := time.Now().UTC()

	mock.ExpectQuery(`(?is)SELECT\s+EXISTS.*event_type\s*=\s*'opened'`).
		WithArgs(uuid.MustParse(testEmailID), now).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))

	mock.ExpectQuery(`(?is)INSERT\s+INTO\s+mailing_open_dedupe`).
		WillReturnError(context.DeadlineExceeded)

	err := c.processOpen(context.Background(), TrackingEvent{
		EventType:    EventOpen,
		OrgID:        testOrgID,
		CampaignID:   testCampaignID,
		SubscriberID: testSubscriberID,
		EmailID:      testEmailID,
		Timestamp:    now,
	})
	// Returns nil — we logged + skipped, did not fail the SQS message.
	// Bubbling the error up would re-deliver and the dedupe gate would
	// fail again on the same row.
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestProcessClick_InsertsRecipientDomain(t *testing.T) {
	c, mock := newConsumer(t)
	now := time.Now().UTC()

	// 1. Idempotency check: derive the deterministic click ID and
	// confirm the consumer asks the DB whether it has already been
	// persisted. We don't assert on the exact UUID since
	// clickDeterministicID is package-private.
	mock.ExpectQuery(`(?is)SELECT\s+EXISTS.*event_type\s*=\s*'clicked'`).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))

	// 2. Subscriber email lookup.
	mock.ExpectQuery(`(?is)SELECT\s+email\s+FROM\s+mailing_subscribers\s+WHERE\s+id\s*=\s*\$1`).
		WithArgs(uuid.MustParse(testSubscriberID)).
		WillReturnRows(sqlmock.NewRows([]string{"email"}).AddRow("foo@yahoo.com"))

	// 3. The INSERT.
	mock.ExpectExec(clickInsertRegex.String()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Aggregate UPDATEs and engagement recompute.
	mock.ExpectExec(`UPDATE\s+mailing_campaigns`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE\s+mailing_subscribers\s+SET\s+total_clicks`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE\s+mailing_inbox_profiles`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`(?is)SELECT\s+COALESCE\(total_opens,0\)`).
		WithArgs(uuid.MustParse(testSubscriberID)).
		WillReturnRows(sqlmock.NewRows([]string{"total_opens", "total_clicks", "total_emails", "last_open_at"}).
			AddRow(0, 5, 50, nil))
	mock.ExpectExec(`UPDATE\s+mailing_subscribers\s+SET\s+engagement_score`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := c.processClick(context.Background(), TrackingEvent{
		EventType:    EventClick,
		OrgID:        testOrgID,
		CampaignID:   testCampaignID,
		SubscriberID: testSubscriberID,
		EmailID:      testEmailID,
		LinkURL:      "https://example.com/landing",
		IPAddress:    "1.2.3.4",
		UserAgent:    "Mozilla/5.0",
		Timestamp:    now,
	})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}
