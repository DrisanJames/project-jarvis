package api

// Journey perf_7d fixtures. What each test PINS:
//   - touch parsing: the " [tN]" campaign-name suffix is the touch number;
//     no suffix = the welcome pass = touch 1 (deployWaveGroups' contract);
//   - ONE query per (vertical, brand) aggregates the lane's wave campaigns
//     by parsed touch, from the denormalized counters — and the result is
//     TTL-cached: the second read issues zero SQL;
//   - the attach path DEGRADES on a failed read: every node keeps perf_7d
//     nil and a named error comes back — a zero row is never substituted
//     for a failed read.

import (
	"context"
	"fmt"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestParseDripTouchFromName(t *testing.T) {
	cases := []struct {
		name string
		want int
	}{
		{"[partner-drip] consumer db 20260822T0400 abcd1234 ffff", 1}, // welcome: no suffix
		{"[partner-drip] consumer db 20260822T0400 abcd1234 ffff [t2]", 2},
		{"[partner-drip] consumer db 20260822T0400 abcd1234 ffff [t14]", 14},
		{"[partner-drip] consumer db something[t2]", 1}, // no space before suffix = not the contract
		{"[partner-drip] consumer db x [t0]", 1},        // touch 0 is invalid → welcome
		{"", 1},
	}
	for _, c := range cases {
		require.Equal(t, c.want, parseDripTouchFromName(c.name), c.name)
	}
}

func TestLaneJourneyPerf7dAggregatesAndCaches(t *testing.T) {
	resetLaneJourneyPerfCacheForTest()
	t.Cleanup(resetLaneJourneyPerfCacheForTest)
	s, mock := newLedgerServiceWithMock(t)

	mock.ExpectQuery(`FROM mailing_campaigns c`).
		WithArgs("org-1", sqlmock.AnyArg(), "consumer", "db").
		WillReturnRows(sqlmock.NewRows([]string{"name", "sent", "delivered", "opened", "clicked"}).
			AddRow("[partner-drip] consumer db 20260821T0400 aaaa1111 ffff", 100, 90, 10, 2).
			AddRow("[partner-drip] consumer db 20260822T0400 bbbb2222 ffff", 50, 45, 5, 1).
			AddRow("[partner-drip] consumer db 20260822T0500 cccc3333 ffff [t2]", 40, 38, 4, 1).
			AddRow("[partner-drip] consumer db 20260822T0600 dddd4444 ffff [t3]", 20, 19, 2, 0))

	byTouch, err := s.laneJourneyPerf7d(context.Background(), "org-1", "consumer", "db")
	require.NoError(t, err)
	require.Equal(t, laneJourneyPerf{Sent: 150, Delivered: 135, Opened: 15, Clicked: 3}, byTouch[1],
		"both welcome waves fold into touch 1")
	require.Equal(t, laneJourneyPerf{Sent: 40, Delivered: 38, Opened: 4, Clicked: 1}, byTouch[2])
	require.Equal(t, laneJourneyPerf{Sent: 20, Delivered: 19, Opened: 2, Clicked: 0}, byTouch[3])
	require.NoError(t, mock.ExpectationsWereMet())

	// Second read: TTL cache — zero SQL (no expectations queued; an
	// unexpected query would error and fail the require.NoError).
	byTouch2, err := s.laneJourneyPerf7d(context.Background(), "org-1", "consumer", "db")
	require.NoError(t, err)
	require.Equal(t, byTouch, byTouch2)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestLaneJourneyAttachPerfDegradesOnError(t *testing.T) {
	resetLaneJourneyPerfCacheForTest()
	t.Cleanup(resetLaneJourneyPerfCacheForTest)
	s, mock := newLedgerServiceWithMock(t)

	mock.ExpectQuery(`FROM mailing_campaigns c`).WillReturnError(fmt.Errorf("boom"))

	touches := []laneJourneyTouch{{Touch: 1}, {Touch: 2}}
	gap := s.laneJourneyAttachPerf(context.Background(), "org-1", "consumer", "db", touches)
	require.Equal(t, "perf read failed", gap)
	require.Nil(t, touches[0].Perf7d, "nil perf on a failed read — never a substituted zero row")
	require.Nil(t, touches[1].Perf7d)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestLaneJourneyAttachPerfFillsEveryTouch(t *testing.T) {
	resetLaneJourneyPerfCacheForTest()
	t.Cleanup(resetLaneJourneyPerfCacheForTest)
	s, mock := newLedgerServiceWithMock(t)

	mock.ExpectQuery(`FROM mailing_campaigns c`).
		WillReturnRows(sqlmock.NewRows([]string{"name", "sent", "delivered", "opened", "clicked"}).
			AddRow("[partner-drip] consumer db x y [t2]", 10, 9, 1, 0))

	touches := []laneJourneyTouch{{Touch: 1}, {Touch: 2}, {Touch: 3}}
	gap := s.laneJourneyAttachPerf(context.Background(), "org-1", "consumer", "db", touches)
	require.Equal(t, "", gap)
	require.NotNil(t, touches[0].Perf7d)
	require.EqualValues(t, 0, touches[0].Perf7d.Sent, "a touch with no waves in-window is a true zero")
	require.EqualValues(t, 10, touches[1].Perf7d.Sent)
	require.NotNil(t, touches[2].Perf7d)
	require.NoError(t, mock.ExpectationsWereMet())
}
