package tracking

// Tests for the SQS consumer's unsubscribe write path (REQ-001, re-scoped
// 2026-07-13: brands are SEPARATE senders).
//
// Before this fix, processUnsubscribe wrote the tracking event, ONE
// subscriber row, the legacy mailing_suppressions table, and SDS — but no
// email-level entry in any store the PMTA send path enforces. A sibling
// subscriber row for the same email IN THE SAME BRAND therefore stayed
// mailable after an unsubscribe (CAN-SPAM exposure).
//
// These tests pin the re-scoped contract:
//
//  1. An unsubscribe produces a BRAND-SCOPED suppression for the
//     ORIGINATING brand (resolved from the campaign's from_email) PLUS the
//     legacy writes.
//  2. Cross-brand mailability is PRESERVED — the operator's exact example:
//     jondoe@gmail.com unsubscribes from discountblog.com; his
//     quizfiesta.com membership stays mailable. Verified against the REAL
//     engine hub (IsSuppressedForBrand), not just a stub.
//  3. The consumer NEVER writes the global suppression set for unsubs.
//  4. A brand-scoped write failure does NOT silently succeed: the error
//     propagates (SQS redelivery retries) and no downstream side effects
//     run — the retry re-runs cleanly because SuppressScoped is idempotent.
//  5. With no suppressor wired (wiring regression), legacy behavior is
//     preserved and the gap is logged at error level.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/engine"
	"github.com/stretchr/testify/require"
)

// stubBrandSuppressor records SuppressScoped calls and returns a scripted error.
type stubBrandSuppressor struct {
	calls     int
	email     string
	brandRoot string
	reason    string
	source    string
	failErr   error
}

func (s *stubBrandSuppressor) SuppressScoped(_ context.Context, email, brandRoot, reason, source, _, _, _ string) error {
	s.calls++
	s.email = email
	s.brandRoot = brandRoot
	s.reason = reason
	s.source = source
	return s.failErr
}

func unsubEvent(ts time.Time) TrackingEvent {
	return TrackingEvent{
		EventType:    EventUnsubscribe,
		OrgID:        testOrgID,
		CampaignID:   testCampaignID,
		SubscriberID: testSubscriberID,
		Timestamp:    ts,
		IPAddress:    "1.2.3.4",
		UserAgent:    "Mozilla/5.0 (X11; Linux x86_64)",
	}
}

// expectUnsubPreamble scripts the two lookups that precede the suppression
// write: subscriber email + campaign from_email (brand resolution).
func expectUnsubPreamble(mock sqlmock.Sqlmock, email, fromEmail string) {
	mock.ExpectQuery(`(?is)SELECT\s+email\s+FROM\s+mailing_subscribers\s+WHERE\s+id\s*=\s*\$1`).
		WithArgs(uuid.MustParse(testSubscriberID)).
		WillReturnRows(sqlmock.NewRows([]string{"email"}).AddRow(email))
	mock.ExpectQuery(`(?is)SELECT\s+COALESCE\(from_email,\s*''\)\s+FROM\s+mailing_campaigns`).
		WithArgs(uuid.MustParse(testCampaignID)).
		WillReturnRows(sqlmock.NewRows([]string{"from_email"}).AddRow(fromEmail))
}

// expectLegacyUnsubWrites scripts the pre-existing unsubscribe side effects
// (tracking event + subscriber flip + campaign counter + legacy suppression
// + the SDS state write).
func expectLegacyUnsubWrites(mock sqlmock.Sqlmock) {
	mock.ExpectExec(`(?is)INSERT\s+INTO\s+mailing_tracking_events.*'unsubscribed'`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`(?is)UPDATE\s+mailing_subscribers\s+SET\s+status\s*=\s*'unsubscribed'`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`(?is)UPDATE\s+mailing_campaigns\s+SET\s+unsubscribe_count`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`(?is)INSERT\s+INTO\s+mailing_suppressions\s*\(`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`(?is)INSERT\s+INTO\s+mailing_subscriber_domain_state`).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

func TestProcessUnsubscribe_WritesBrandScopedAndLegacySuppression(t *testing.T) {
	c, mock := newConsumer(t)
	stub := &stubBrandSuppressor{}
	c.brandSuppressor = stub

	expectUnsubPreamble(mock, "Foo@Gmail.com", "newsletter@em.discountblog.com")
	expectLegacyUnsubWrites(mock)

	err := c.processUnsubscribe(context.Background(), unsubEvent(time.Now().UTC()))
	require.NoError(t, err)

	// The brand-scoped (enforced) store was written for the ORIGINATING
	// brand only…
	require.Equal(t, 1, stub.calls, "brand suppressor must be called exactly once")
	require.Equal(t, "Foo@Gmail.com", stub.email)
	require.Equal(t, "discountblog.com", stub.brandRoot, "brand root must resolve from the campaign's sending domain")
	require.Equal(t, "user_unsubscribe", stub.reason)
	require.Equal(t, "sqs_tracking", stub.source)

	// …AND the legacy writes still ran (expectations include the legacy
	// mailing_suppressions INSERT and the SDS state write).
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestProcessUnsubscribe_CrossBrandMailabilityPreserved is the operator's
// exact acceptance example, run against the REAL engine hub:
// jondoe@gmail.com unsubscribes from a discountblog.com campaign — only the
// DB unsubscribe is honored; his quizfiesta.com membership stays mailable,
// and the GLOBAL suppression set is never touched.
func TestProcessUnsubscribe_CrossBrandMailabilityPreserved(t *testing.T) {
	c, mock := newConsumer(t)

	hubDB, hubMock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { hubDB.Close() })
	hub := engine.NewGlobalSuppressionHub(hubDB, testOrgID, "")
	c.brandSuppressor = hub

	expectUnsubPreamble(mock, "jondoe@gmail.com", "deals@em.discountblog.com")
	// The hub writes the brand-scoped table — and ONLY that table.
	hubMock.ExpectExec(`(?is)INSERT\s+INTO\s+mailing_domain_suppressions`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectLegacyUnsubWrites(mock)

	require.NoError(t, c.processUnsubscribe(context.Background(), unsubEvent(time.Now().UTC())))

	// Originating brand: suppressed.
	require.True(t, hub.IsSuppressedForBrand("jondoe@gmail.com", "discountblog.com"),
		"the unsubscribing brand must enforce the unsub")
	// Sibling brand: STILL MAILABLE (brands are separate senders).
	require.False(t, hub.IsSuppressedForBrand("jondoe@gmail.com", "quizfiesta.com"),
		"a sibling brand must NOT be suppressed by another brand's unsubscribe")
	// Global set: untouched — the consumer must never write it for unsubs.
	require.False(t, hub.IsSuppressed("jondoe@gmail.com"),
		"unsubscribe must never enter the GLOBAL suppression set")
	require.Equal(t, 0, hub.Count(), "global email set must stay empty")

	require.NoError(t, mock.ExpectationsWereMet())
	require.NoError(t, hubMock.ExpectationsWereMet(), "hub must write mailing_domain_suppressions and nothing else")
}

func TestProcessUnsubscribe_BrandWriteFailureReturnsErrorForRetry(t *testing.T) {
	c, mock := newConsumer(t)
	stub := &stubBrandSuppressor{failErr: errors.New("db down")}
	c.brandSuppressor = stub

	// Only the two lookups run — the failure must return BEFORE any
	// side-effect write so SQS redelivery retries the whole event cleanly.
	expectUnsubPreamble(mock, "foo@gmail.com", "deals@em.discountblog.com")

	err := c.processUnsubscribe(context.Background(), unsubEvent(time.Now().UTC()))
	require.Error(t, err, "brand-suppression write failure must propagate so the SQS message is retried, never silently succeed")
	require.ErrorIs(t, err, stub.failErr)
	require.Equal(t, 1, stub.calls)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestProcessUnsubscribe_NilSuppressorStillWritesLegacy(t *testing.T) {
	c, mock := newConsumer(t) // no suppressor wired — wiring-regression path

	expectUnsubPreamble(mock, "foo@gmail.com", "deals@em.discountblog.com")
	expectLegacyUnsubWrites(mock)

	err := c.processUnsubscribe(context.Background(), unsubEvent(time.Now().UTC()))
	require.NoError(t, err, "nil hub must not break unsubscribe processing (it is logged at error level)")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestProcessUnsubscribe_UnresolvedBrandSkipsScopedWriteButContinues(t *testing.T) {
	c, mock := newConsumer(t)
	stub := &stubBrandSuppressor{}
	c.brandSuppressor = stub

	// Campaign lookup fails → sending domain (and brand root) unresolved.
	mock.ExpectQuery(`(?is)SELECT\s+email\s+FROM\s+mailing_subscribers\s+WHERE\s+id\s*=\s*\$1`).
		WithArgs(uuid.MustParse(testSubscriberID)).
		WillReturnRows(sqlmock.NewRows([]string{"email"}).AddRow("foo@gmail.com"))
	mock.ExpectQuery(`(?is)SELECT\s+COALESCE\(from_email,\s*''\)\s+FROM\s+mailing_campaigns`).
		WithArgs(uuid.MustParse(testCampaignID)).
		WillReturnError(errors.New("campaign gone"))

	// Legacy writes still run — minus the SDS write (empty domain skips it).
	mock.ExpectExec(`(?is)INSERT\s+INTO\s+mailing_tracking_events.*'unsubscribed'`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`(?is)UPDATE\s+mailing_subscribers\s+SET\s+status\s*=\s*'unsubscribed'`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`(?is)UPDATE\s+mailing_campaigns\s+SET\s+unsubscribe_count`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`(?is)INSERT\s+INTO\s+mailing_suppressions\s*\(`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := c.processUnsubscribe(context.Background(), unsubEvent(time.Now().UTC()))
	require.NoError(t, err, "an unresolvable brand must not poison-loop the message")
	require.Equal(t, 0, stub.calls, "no brand-scoped write may run without a resolved brand root")
	require.NoError(t, mock.ExpectationsWereMet())
}
