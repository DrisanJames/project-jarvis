package worker

// WP-A (2026-11-04): when EVERY partner dataset is emergency-paused, tickOnce
// must stop after the lease and issue no partner_clean_queue statement — while
// still stamping the mediator tick and the ladder heartbeat. One unpaused
// dataset, or PARTNER_DRIP_PAUSE_SHORTCIRCUIT_DISABLED=1, restores the full tick.

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ignite/sparkpost-monitor/internal/engine"
	"github.com/ignite/sparkpost-monitor/internal/worker/dripsupply"
)

// pauseProbeRecorder captures every SQL string sqlmock is asked to match, so
// the test can assert on what the tick actually sent to the driver.
type pauseProbeRecorder struct {
	mu   sync.Mutex
	seen []string
}

func (r *pauseProbeRecorder) matcher() sqlmock.QueryMatcher {
	return sqlmock.QueryMatcherFunc(func(expected, actual string) error {
		r.mu.Lock()
		r.seen = append(r.seen, actual)
		r.mu.Unlock()
		re, err := regexp.Compile(expected)
		if err != nil {
			return err
		}
		if !re.MatchString(actual) {
			return errors.New("no match")
		}
		return nil
	})
}

func (r *pauseProbeRecorder) any(substr string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.seen {
		if strings.Contains(s, substr) {
			return true
		}
	}
	return false
}

// runPauseTick drives one tickOnce. probe=nil means the probe is not expected
// (kill switch). Generous catch-all scaffolding is registered in every case so
// the full tick can reach its partner_clean_queue reads; a missing gate would
// therefore show up as a recorded pcq statement, never pass vacuously.
func runPauseTick(t *testing.T, probe *bool) (*pauseProbeRecorder, *dripsupply.Mediator) {
	t.Helper()
	t.Setenv("PARTNER_DRIP_TICK_LOCK_DISABLED", "1")
	rec := &pauseProbeRecorder{}
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(rec.matcher()))
	require.NoError(t, err)
	defer db.Close()
	mock.MatchExpectationsInOrder(false)

	if probe != nil {
		mock.ExpectQuery(regexp.QuoteMeta(partnerDripAllPausedProbeSQL)).
			WillReturnRows(sqlmock.NewRows([]string{"all_paused"}).AddRow(*probe))
	}
	mock.ExpectExec(`mailing_worker_heartbeats`).WillReturnResult(sqlmock.NewResult(0, 1))
	for i := 0; i < 200; i++ {
		mock.ExpectBegin()
		mock.ExpectCommit()
		mock.ExpectRollback()
		mock.ExpectExec(`(?s).*`).WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectQuery(`(?s).*`).WillReturnError(errors.New("not mocked"))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	po := &PartnerDripOrchestrator{
		db:  db,
		ctx: ctx,
		cfg: PartnerDripOrchestratorConfig{
			TickInterval:         15 * time.Minute,
			BrandsPerTick:        1,
			ClaimedJanitorMaxAge: 45 * time.Minute,
			FollowupDisabled:     true,
			GovernedDisabled:     true,
			DeployFn: func(context.Context, engine.PMTACampaignInput) (string, error) {
				return "", nil
			},
		},
	}
	med := dripsupply.NewMediator(db, dripsupply.NewService(db),
		dripsupply.MediatorConfig{Mode: dripsupply.ModeOff, AlertsDisabled: true})
	po.SetCapacityMediator(med)
	po.tickOnce()
	return rec, med
}

func TestPartnerDripPausedShortCircuit_AllPaused(t *testing.T) {
	yes := true
	rec, med := runPauseTick(t, &yes)
	assert.True(t, rec.any("partner_datasets WHERE NOT COALESCE(paused_emergency"), "probe must run")
	assert.False(t, rec.any("partner_clean_queue"), "all paused: no partner_clean_queue statement may be issued")
	assert.True(t, rec.any("mailing_worker_heartbeats"), "ladder heartbeat must still be emitted")
	assert.False(t, med.Tick().IsZero(), "mediator TickStart must still run")
}

func TestPartnerDripPausedShortCircuit_OneUnpaused(t *testing.T) {
	no := false
	rec, _ := runPauseTick(t, &no)
	assert.True(t, rec.any("partner_datasets WHERE NOT COALESCE(paused_emergency"), "probe must run")
	assert.True(t, rec.any("partner_clean_queue"), "unpaused: the existing full tick must run")
}

func TestPartnerDripPausedShortCircuit_KillSwitch(t *testing.T) {
	t.Setenv("PARTNER_DRIP_PAUSE_SHORTCIRCUIT_DISABLED", "1")
	rec, _ := runPauseTick(t, nil)
	assert.False(t, rec.any("partner_datasets WHERE NOT COALESCE(paused_emergency"), "kill switch: probe must not run")
	assert.True(t, rec.any("partner_clean_queue"), "kill switch: the existing full tick must run")
}

func TestPartnerDripPausedShortCircuit_ProbeErrorFailsOpen(t *testing.T) {
	t.Setenv("PARTNER_DRIP_TICK_LOCK_DISABLED", "1")
	rec := &pauseProbeRecorder{}
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(rec.matcher()))
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery(regexp.QuoteMeta(partnerDripAllPausedProbeSQL)).WillReturnError(errors.New("pq: statement timeout"))
	po := &PartnerDripOrchestrator{db: db, ctx: context.Background()}
	assert.False(t, po.allDatasetsPausedShortCircuit(), "probe error must fail OPEN")
	require.NoError(t, mock.ExpectationsWereMet())
}
