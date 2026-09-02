package tracking

// REQ-085b regression guards — organization_id must never be written as
// uuid.Nil.
//
// The defect: internal/tracking/handler.go HandleOfferRedirect published its
// click telemetry with `OrgID: ""` (the /o/ path contract carries
// brand/subscriber/hash/campaign and no org), and consumer.go did
// `orgID, _ := uuid.Parse(evt.OrgID)` — discarding the error and leaving
// uuid.Nil. Every one of those rows landed in mailing_tracking_events with
// organization_id = '00000000-0000-0000-0000-000000000000' and was invisible
// to every org-scoped reader, including the nightly lake loader
// (backfill_to_lake.py --source tracking filters organization_id = '…0001').
//
// Measured in prod on 2026-08-31: 7,909 of 57,152 'clicked' rows (13.8%) —
// all of them /o/ redirects; 100% carried the renderOfferDestination
// `source_id=email` signature and 2,113 carried a `sub2=t.<host>` value only
// brandRootFromHost can produce. Opens were 0/2,662,793 affected, because the
// /track/open pixel token carries the org in parts[0].
//
// Two guards, both directions:
//   - an empty/malformed org resolves to SingleTenantFallbackOrgID;
//   - a VALID org is passed through untouched (negative control) — a fix that
//     stamped the fallback unconditionally would break the day this deployment
//     stops being single-tenant.

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// resetOrgFallbackLimiter clears the once-per-minute log limiter so each test
// starts from a known state.
func resetOrgFallbackLimiter() {
	orgFallbackMu.Lock()
	orgFallbackLastLogged = time.Time{}
	orgFallbackSuppressed = 0
	orgFallbackMu.Unlock()
}

// --- DoD 1: the consumer resolves, never Nil -------------------------------

func TestResolveOrgID_FallbackAndPassthrough(t *testing.T) {
	fallback := uuid.MustParse(SingleTenantFallbackOrgID)
	valid := uuid.MustParse(testOrgID)

	cases := []struct {
		name string
		raw  string
		want uuid.UUID
	}{
		{"empty", "", fallback},
		{"whitespace", "   ", fallback},
		{"malformed", "not-a-uuid", fallback},
		// An explicit all-zeros org id is the same bug arriving pre-parsed:
		// uuid.Parse SUCCEEDS on it, so the error check alone would let it
		// through. It must land on the fallback too.
		{"explicit uuid.Nil", "00000000-0000-0000-0000-000000000000", fallback},
		// Negative control — a real org must survive untouched.
		{"valid org passthrough", testOrgID, valid},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetOrgFallbackLimiter()
			got := resolveOrgID(tc.raw, EventClick)
			require.Equal(t, tc.want, got)
			require.NotEqual(t, uuid.Nil, got, "organization_id must never be written as uuid.Nil")
		})
	}
}

func TestResolveDefaultOrgID_PrefersEnvThenConstant(t *testing.T) {
	// Mirrors steps 4 and 6 of api.GetOrgIDFromRequest (org_context.go:296).
	t.Setenv("DEFAULT_ORG_ID", "")
	require.Equal(t, uuid.MustParse(SingleTenantFallbackOrgID), resolveDefaultOrgID())

	t.Setenv("DEFAULT_ORG_ID", testOrgID)
	require.Equal(t, uuid.MustParse(testOrgID), resolveDefaultOrgID())

	// A junk env value must not poison the org — fall back to the constant.
	t.Setenv("DEFAULT_ORG_ID", "garbage")
	require.Equal(t, uuid.MustParse(SingleTenantFallbackOrgID), resolveDefaultOrgID())
}

// TestLogOrgFallback_RateLimitedToOncePerMinute proves the log line the DoD
// asks for is emitted, and that a burst of bad events collapses into one line
// carrying the suppressed count — 8k lines/day would drown the signal.
func TestLogOrgFallback_RateLimitedToOncePerMinute(t *testing.T) {
	resetOrgFallbackLimiter()

	var buf bytes.Buffer
	prev := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prev)
		log.SetFlags(prevFlags)
	})

	for i := 0; i < 50; i++ {
		require.Equal(t, uuid.MustParse(SingleTenantFallbackOrgID), resolveOrgID("", EventClick))
	}

	lines := strings.Count(buf.String(), "unusable org_id")
	require.Equal(t, 1, lines, "expected exactly one log line per minute, got:\n%s", buf.String())

	// Force the window open: the next call logs again and reports the 49
	// events it swallowed.
	orgFallbackMu.Lock()
	orgFallbackLastLogged = time.Now().Add(-2 * time.Minute)
	orgFallbackMu.Unlock()

	buf.Reset()
	resolveOrgID("", EventClick)
	require.Contains(t, buf.String(), "49 similar suppressed in the last minute")
}

// --- DoD 1: the write path actually carries the fallback -------------------

// expectClickWrite sets up the full processClick expectation chain, asserting
// the organization_id bound to the tracking_events INSERT is wantOrg.
func expectClickWrite(t *testing.T, mock sqlmock.Sqlmock, wantOrg uuid.UUID, now time.Time) {
	t.Helper()

	mock.ExpectQuery(`(?is)SELECT\s+EXISTS.*event_type\s*=\s*'clicked'`).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))

	mock.ExpectQuery(`(?is)SELECT\s+email\s+FROM\s+mailing_subscribers\s+WHERE\s+id\s*=\s*\$1`).
		WithArgs(uuid.MustParse(testSubscriberID)).
		WillReturnRows(sqlmock.NewRows([]string{"email"}).AddRow("clicker@hotmail.com"))

	// THE ASSERTION: arg 2 of the INSERT is the organization_id. sqlmock
	// fails the test if it is anything but wantOrg — in particular if it is
	// uuid.Nil, which is what shipped.
	mock.ExpectExec(clickInsertRegex.String()).
		WithArgs(
			sqlmock.AnyArg(), // clickDeterministicID
			wantOrg,
			uuid.MustParse(testCampaignID),
			uuid.MustParse(testSubscriberID),
			now,
			"9.9.9.9",
			uaBrowser,
			"desktop",
			"https://www.codefortwo.com/K4C5ZLC/PS8241/?source_id=email",
			false, // isMachineClick
		).
		// RowsAffected 0 short-circuits the aggregate UPDATEs — this test is
		// about the org binding, not the counters.
		WillReturnResult(sqlmock.NewResult(0, 0))
}

func TestProcessClick_OrgFallback_EmptyOrgWritesSingleTenantOrg(t *testing.T) {
	resetOrgFallbackLimiter()
	c, mock := newConsumer(t)
	now := time.Now().UTC()

	expectClickWrite(t, mock, uuid.MustParse(SingleTenantFallbackOrgID), now)

	err := c.processClick(context.Background(), TrackingEvent{
		EventType:    EventClick,
		OrgID:        "", // what HandleOfferRedirect used to publish
		CampaignID:   testCampaignID,
		SubscriberID: testSubscriberID,
		LinkURL:      "https://www.codefortwo.com/K4C5ZLC/PS8241/?source_id=email",
		IPAddress:    "9.9.9.9",
		UserAgent:    uaBrowser,
		Timestamp:    now,
	})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

// Negative control: a valid org id is written verbatim.
func TestProcessClick_OrgFallback_ValidOrgIsPreserved(t *testing.T) {
	resetOrgFallbackLimiter()
	c, mock := newConsumer(t)
	now := time.Now().UTC()

	expectClickWrite(t, mock, uuid.MustParse(testOrgID), now)

	err := c.processClick(context.Background(), TrackingEvent{
		EventType:    EventClick,
		OrgID:        testOrgID,
		CampaignID:   testCampaignID,
		SubscriberID: testSubscriberID,
		LinkURL:      "https://www.codefortwo.com/K4C5ZLC/PS8241/?source_id=email",
		IPAddress:    "9.9.9.9",
		UserAgent:    uaBrowser,
		Timestamp:    now,
	})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

// The open path shares resolveOrgID; a malformed token must not write Nil
// there either (60 'unsubscribed' rows in prod carry the zero org from the
// same class of malformed token).
func TestProcessOpen_OrgFallback_MalformedOrgWritesSingleTenantOrg(t *testing.T) {
	resetOrgFallbackLimiter()
	c, mock := newConsumer(t)
	now := time.Now().UTC()

	mock.ExpectQuery(`(?is)SELECT\s+EXISTS.*event_type\s*=\s*'opened'`).
		WithArgs(uuid.MustParse(testEmailID), now).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectQuery(`(?is)INSERT\s+INTO\s+mailing_open_dedupe`).
		WithArgs(uuid.MustParse(testCampaignID), uuid.MustParse(testSubscriberID), uuid.MustParse(testEmailID), now).
		WillReturnRows(sqlmock.NewRows([]string{"true"}).AddRow(true))
	mock.ExpectQuery(`(?is)SELECT\s+email\s+FROM\s+mailing_subscribers`).
		WithArgs(uuid.MustParse(testSubscriberID)).
		WillReturnRows(sqlmock.NewRows([]string{"email"}).AddRow("opener@aol.com"))
	mock.ExpectQuery(`(?is)SELECT\s+event_at.*event_type\s*=\s*'sent'`).
		WithArgs(uuid.MustParse(testSubscriberID), uuid.MustParse(testCampaignID)).
		WillReturnRows(sqlmock.NewRows([]string{"event_at"}))

	mock.ExpectExec(openInsertRegex.String()).
		WithArgs(
			uuid.MustParse(testEmailID),
			uuid.MustParse(SingleTenantFallbackOrgID),
			uuid.MustParse(testCampaignID),
			uuid.MustParse(testSubscriberID),
			now,
			"1.2.3.4",
			uaBrowser,
			"desktop",
			false,
		).
		WillReturnResult(sqlmock.NewResult(0, 0))

	err := c.processOpen(context.Background(), TrackingEvent{
		EventType:    EventOpen,
		OrgID:        "org-is-not-a-uuid",
		CampaignID:   testCampaignID,
		SubscriberID: testSubscriberID,
		EmailID:      testEmailID,
		IPAddress:    "1.2.3.4",
		UserAgent:    uaBrowser,
		Timestamp:    now,
	})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

// --- DoD 2: the PRODUCER no longer emits an empty org ----------------------

// TestOfferRedirect_PublishesResolvedOrgID is the guard at the source. If
// anyone reverts handler.go to `OrgID: ""` this fails, and it fails on both
// route shapes (4-segment legacy links already in inboxes, and the
// 5-segment brand-in-path mint).
func TestOfferRedirect_PublishesResolvedOrgID(t *testing.T) {
	dict := stubDict(map[string]smartLinkEntry{
		"abc123": {
			Destination: "https://www.codefortwo.com/K4C5ZLC/PS8241/?sub1={{subscriber.id}}&sub2={{brand.domain}}",
			RiskProfile: "low",
			BrandRoot:   "discountblog.com",
		},
	})

	t.Run("legacy 4-segment", func(t *testing.T) {
		pub := &capturePublisher{}
		h := NewHandler(pub, dict)
		rec := doOffer(h, "t.em.discountblog.com", subUUID, "abc123", campUUID, uaBrowser)
		require.Equal(t, http.StatusFound, rec.Code)

		evt, ok := pub.last()
		require.True(t, ok, "offer redirect must publish a click event")
		require.NotEmpty(t, evt.OrgID, "REQ-085b: /o/ must not publish an empty org_id")
		require.Equal(t, defaultOrgID.String(), evt.OrgID)
		// And it must survive the consumer's resolver as a real org.
		require.NotEqual(t, uuid.Nil, resolveOrgID(evt.OrgID, evt.EventType))
	})

	t.Run("brand-in-path 5-segment", func(t *testing.T) {
		pub := &capturePublisher{}
		h := NewHandler(pub, dict)
		rec := doOffer5(h, "projectjarvis.io", "discountblog.com", subUUID, "abc123", campUUID, uaBrowser)
		require.Equal(t, http.StatusFound, rec.Code)

		evt, ok := pub.last()
		require.True(t, ok)
		require.Equal(t, defaultOrgID.String(), evt.OrgID)
	})
}
