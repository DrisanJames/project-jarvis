package worker

// Pins the reactive stall-alerting contract (operator 2026-08-09): a stall
// older than muteAfter gets exactly ONE final WATCH notice and is then muted;
// the regular re-alert claim skips muted rows; a resumed heartbeat clears the
// mute. Negative path first — before this, retired jobs (converter_autosender,
// send_day_bridge) re-alerted hourly for 11+ days.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

type captureNotifier struct {
	mu     sync.Mutex
	titles []string
	bodies []string
}

func (c *captureNotifier) Notify(title, body string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.titles = append(c.titles, title)
	c.bodies = append(c.bodies, body)
	return nil
}
func (c *captureNotifier) Name() string { return "capture" }

func TestCheckOnceMutesLongStallsAndSkipsThemInClaim(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	n := &captureNotifier{}
	m := NewWorkerHealthMonitor(db, n)

	// 1. mark-stalled pass.
	mock.ExpectExec(`SET stalled = TRUE`).
		WillReturnResult(sqlmock.NewResult(0, 2))
	// 2. mute pass claims the long-dead worker (one final notice).
	mock.ExpectQuery(`SET alerts_muted = TRUE`).
		WithArgs(int(m.muteAfter.Seconds())).
		WillReturnRows(sqlmock.NewRows([]string{"worker_name", "last_beat_at"}).
			AddRow("job:converter_autosender", time.Now().Add(-272*time.Hour)))
	// 3. re-alert claim MUST exclude muted rows.
	mock.ExpectQuery(`alerts_muted = FALSE`).
		WithArgs(int(m.reAlertAfter.Seconds())).
		WillReturnRows(sqlmock.NewRows([]string{
			"worker_name", "last_beat_at", "expected_interval_seconds", "last_status", "coalesce"}))

	m.checkOnce(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("query order/shape: %v", err)
	}
	if len(n.titles) != 1 {
		t.Fatalf("want exactly 1 mute notice, got %d: %v", len(n.titles), n.titles)
	}
	if !strings.Contains(n.titles[0], "muted") || !strings.Contains(n.titles[0], "job:converter_autosender") {
		t.Fatalf("mute notice missing worker/mute wording: %q", n.titles[0])
	}
}

func TestEmitHeartbeatClearsMuteOnBeat(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(`alerts_muted\s+= FALSE`).
		WithArgs("job:x", "ok", nil, 3600).
		WillReturnResult(sqlmock.NewResult(0, 1))

	EmitHeartbeat(context.Background(), db, "job:x", 3600, "ok", "")

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("heartbeat upsert must clear alerts_muted: %v", err)
	}
}
