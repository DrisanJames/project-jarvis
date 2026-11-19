package worker

// (h) Rate-collapse alarm: queue_ready_rows > 100k AND sent_last_15m below
// SEND_RATE_FLOOR_15M on 2 consecutive MEASURED samples pages once. A probe
// error is UNKNOWN — never a page.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

// floorTick registers one full runOnce with the given ready depth and sent
// count. thruErr makes the throughput probe fail.
func floorTick(mock sqlmock.Sqlmock, ready, sent int64, thruErr bool) {
	expectJanitorTx(mock, 0)
	expectCheckTx(mock, reSubmitting, sqlmock.NewRows([]string{"count", "oldest"}).AddRow(int64(0), int64(0)))
	expectCheckTx(mock, reDeadLetter, sqlmock.NewRows([]string{"c"}).AddRow(int64(0)))
	expectCheckTx(mock, reBacklog, sqlmock.NewRows([]string{"c"}).AddRow(ready))
	if ready > queuedBacklogThreshold {
		// draining → the backlog invariant itself does not page
		expectCheckTx(mock, reBacklogProgress, sqlmock.NewRows([]string{"exists"}).AddRow(true))
	}
	expectCheckTx(mock, reOldest, sqlmock.NewRows([]string{"age"}).AddRow(int64(0)))
	expectCheckTx(mock, reWaveUnlanded, cleanWave())
	expectCheckTx(mock, reCampaignNoSend, cleanNoSend())
	expectCheckTx(mock, reFailedBurst, cleanBurst())
	expectCheckTx(mock, reScheduledDead, cleanDead())
	if thruErr {
		expectCheckTxErr(mock, reThroughput)
	} else {
		expectCheckTx(mock, reThroughput, sqlmock.NewRows([]string{"n", "last"}).AddRow(sent, time.Now()))
	}
}

type floorSample struct {
	ready, sent int64
	thruErr     bool
}

func runFloor(t *testing.T, samples ...floorSample) []capturedSMS {
	t.Helper()
	db, mock := newSelfCheckMockDB(t)
	sc := NewOutboxSelfCheck(db)
	alerter := &capturingAlerter{}
	sc.SetAlerter(alerter, []string{"+15555550100"})
	for _, s := range samples {
		floorTick(mock, s.ready, s.sent, s.thruErr)
		sc.runOnce(context.Background())
	}
	require.NoError(t, mock.ExpectationsWereMet())
	var out []capturedSMS
	for _, m := range alerter.drain() {
		if strings.Contains(m.Body, "send rate collapsed") {
			out = append(out, m)
		}
	}
	return out
}

func TestSendRateFloor_H(t *testing.T) {
	t.Run("two_breaching_samples_one_page", func(t *testing.T) {
		got := runFloor(t, floorSample{ready: 700000, sent: 5000}, floorSample{ready: 700000, sent: 5000}, floorSample{ready: 700000, sent: 5000})
		require.Len(t, got, 1, "page once (re-alert window suppresses the third)")
		require.Contains(t, got[0].Body, "*5,000* sent in 15m")
		require.Contains(t, got[0].Body, "Ready: 700,000 rows")
		require.Contains(t, got[0].Body, "Floor: 15,000/15m")
	})
	t.Run("first_sample_only_no_page", func(t *testing.T) {
		require.Empty(t, runFloor(t, floorSample{ready: 700000, sent: 5000}))
	})
	t.Run("ready_zero_no_page", func(t *testing.T) {
		require.Empty(t, runFloor(t, floorSample{ready: 0, sent: 5000}, floorSample{ready: 0, sent: 5000}))
	})
	t.Run("healthy_rate_no_page", func(t *testing.T) {
		require.Empty(t, runFloor(t, floorSample{ready: 700000, sent: 40000}, floorSample{ready: 700000, sent: 40000}))
	})
	t.Run("probe_error_is_unknown_no_page", func(t *testing.T) {
		require.Empty(t, runFloor(t,
			floorSample{ready: 700000, sent: 5000},
			floorSample{ready: 700000, thruErr: true},
			floorSample{ready: 700000, sent: 5000}),
			"an UNKNOWN sample breaks the streak and never pages")
	})
	t.Run("kill_switch_off", func(t *testing.T) {
		t.Setenv("SEND_RATE_FLOOR_ALERT", "off")
		require.Empty(t, runFloor(t, floorSample{ready: 700000, sent: 5000}, floorSample{ready: 700000, sent: 5000}))
	})
	t.Run("floor_env", func(t *testing.T) {
		t.Setenv("SEND_RATE_FLOOR_15M", "4000")
		require.Empty(t, runFloor(t, floorSample{ready: 700000, sent: 5000}, floorSample{ready: 700000, sent: 5000}))
	})
}
