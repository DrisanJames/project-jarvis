package worker

// ISP dispatcher resilience (2026-11-18 SES stall: one 5s ctx over the scan,
// per-campaign refreshes and admissions; truncated scans evicted live
// campaigns; partial reads zeroed remaining → "fully dispatched" → evict →
// cooldown → re-admit). Every read is now complete-or-error (dbscan) and a
// failed read keeps last known good.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

const (
	reDispScan    = `FROM mailing_campaigns\s+WHERE status = 'sending'\s+AND esp_quotas IS NOT NULL`
	reDispOrphan  = `esp_quotas IS NULL OR NOT`
	reDispBulk    = `campaign_id = ANY\(\$1\)\s+AND status = 'queued'\s+GROUP BY campaign_id`
	reDispDerive  = `GROUP BY recipient_isp`
	reDispRefresh = `WHERE campaign_id = \$1\s+AND status = 'queued'\s+GROUP BY COALESCE`
	reDispClaim   = `WITH claimed AS`

	quotasGmail10 = `{"isp_quotas":[{"isp":"gmail","volume":10}]}`
	quotasVol0    = `{"isp_quotas":[{"isp":"gmail","volume":0}]}`
)

var errDispTimeout = errors.New("pq: canceling statement due to statement timeout")

func newDispatchPool(t *testing.T) (*SendWorkerPool, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return &SendWorkerPool{db: db, ctx: context.Background(), batchSize: 100}, mock
}

func scanRows(ids ...string) *sqlmock.Rows {
	r := sqlmock.NewRows([]string{"id", "q"})
	for _, id := range ids {
		q := quotasGmail10
		if id == "camp-new" {
			q = quotasVol0
		}
		r.AddRow(id, q)
	}
	return r
}

func ispRows(kv ...any) *sqlmock.Rows {
	r := sqlmock.NewRows([]string{"isp", "n"})
	for i := 0; i < len(kv); i += 2 {
		r.AddRow(kv[i], kv[i+1])
	}
	return r
}

func orphanRows() *sqlmock.Rows { return sqlmock.NewRows([]string{"id", "name"}) }

func tracked(id string, rem map[string]int) *ispCampaignState {
	plan := map[string]int{}
	for isp := range rem {
		plan[isp] = 100
	}
	return &ispCampaignState{campaignID: id, plan: plan, remaining: rem, lastRefreshOK: true, lastRefreshAt: time.Now()}
}

type dispCounters struct{ scans, evict, admit, claim int64 }

func readDispCounters() dispCounters {
	return dispCounters{dispScansFailed.Load(), dispEvictions.Load(), dispAdmitFailed.Load(), dispClaimErrors.Load()}
}

// (a) A tracked campaign's refresh is slow (bulk ctx expires) while a new
// volume-0 campaign appears in the scan: the new campaign must be admitted on
// its own ctx, and nothing is evicted.
func TestISPDispatch_A_AdmitsNewCampaignWhileTrackedRefreshSlow(t *testing.T) {
	p, mock := newDispatchPool(t)
	p.ispBulkTimeout = 100 * time.Millisecond
	before := readDispCounters()

	mock.ExpectQuery(reDispScan).WillReturnRows(scanRows("camp-a", "camp-new"))
	mock.ExpectQuery(reDispBulk).WillDelayFor(2 * time.Second).
		WillReturnRows(sqlmock.NewRows([]string{"c", "isp", "n"}))
	mock.ExpectQuery(reDispDerive).WithArgs("camp-new").WillReturnRows(ispRows("gmail", 7))
	mock.ExpectQuery(reDispRefresh).WithArgs("camp-new").WillReturnRows(ispRows("gmail", 7))
	mock.ExpectQuery(reDispOrphan).WillReturnRows(orphanRows())

	states := map[string]*ispCampaignState{"camp-a": tracked("camp-a", map[string]int{"gmail": 500})}
	start := time.Now()
	p.refreshISPCampaigns(states, map[string]time.Time{})

	require.Less(t, time.Since(start), time.Second, "a slow bulk refresh must cost at most its ctx")
	require.Contains(t, states, "camp-new", "new campaign must be admitted despite the slow tracked refresh")
	require.Equal(t, 7, states["camp-new"].remaining["gmail"])
	require.Contains(t, states, "camp-a")
	require.Equal(t, 500, states["camp-a"].remaining["gmail"], "failed refresh keeps last known good")
	require.False(t, states["camp-a"].lastRefreshOK)
	require.Equal(t, before.evict, dispEvictions.Load())
	require.NoError(t, mock.ExpectationsWereMet())
}

// (a2) The auto-derive read fails: campaign NOT admitted with a partial plan,
// counted, and admitted on the next refresh.
func TestISPDispatch_A2_DeriveFailureSkipsAndRetries(t *testing.T) {
	p, mock := newDispatchPool(t)
	before := readDispCounters()
	cooldown := map[string]time.Time{}
	states := map[string]*ispCampaignState{}

	mock.ExpectQuery(reDispScan).WillReturnRows(scanRows("camp-new"))
	mock.ExpectQuery(reDispDerive).WillReturnRows(ispRows("gmail", 7, "yahoo", 3).RowError(1, errDispTimeout))
	mock.ExpectQuery(reDispOrphan).WillReturnRows(orphanRows())
	p.refreshISPCampaigns(states, cooldown)
	require.NotContains(t, states, "camp-new", "partial derive must not admit a partial plan")
	require.NotContains(t, cooldown, "camp-new", "a failed read is not 'zero queued'")
	require.Equal(t, before.admit+1, dispAdmitFailed.Load())

	mock.ExpectQuery(reDispScan).WillReturnRows(scanRows("camp-new"))
	mock.ExpectQuery(reDispDerive).WillReturnRows(ispRows("gmail", 7, "yahoo", 3))
	mock.ExpectQuery(reDispRefresh).WillReturnRows(ispRows("gmail", 7, "yahoo", 3))
	mock.ExpectQuery(reDispOrphan).WillReturnRows(orphanRows())
	p.refreshISPCampaigns(states, cooldown)
	require.Contains(t, states, "camp-new")
	require.Equal(t, map[string]int{"gmail": 7, "yahoo": 3}, states["camp-new"].plan)
	require.NoError(t, mock.ExpectationsWereMet())
}

// (b) RowError mid-scan (and a Scan error): no eviction, no admission, state
// unchanged, scans_failed++.
func TestISPDispatch_B_ScanErrorKeepsState(t *testing.T) {
	for name, rows := range map[string]*sqlmock.Rows{
		"row_error":  scanRows("camp-a", "camp-b").RowError(1, errDispTimeout),
		"scan_error": sqlmock.NewRows([]string{"id", "q"}).AddRow(nil, `{}`),
	} {
		t.Run(name, func(t *testing.T) {
			p, mock := newDispatchPool(t)
			p.ispLastBulkRefresh = time.Now() // isolate the scan
			before := readDispCounters()
			mock.ExpectQuery(reDispScan).WillReturnRows(rows)

			states := map[string]*ispCampaignState{
				"camp-a": tracked("camp-a", map[string]int{"gmail": 5}),
				"camp-z": tracked("camp-z", map[string]int{"gmail": 9}),
			}
			states["camp-z"].missedScans = 1
			p.refreshISPCampaigns(states, map[string]time.Time{})

			require.Len(t, states, 2)
			require.Equal(t, 9, states["camp-z"].remaining["gmail"])
			require.Equal(t, 1, states["camp-z"].missedScans, "a failed scan neither counts nor resets a miss")
			require.NotContains(t, states, "camp-b", "no admission off a failed scan")
			require.Equal(t, before.scans+1, dispScansFailed.Load())
			require.Equal(t, before.evict, dispEvictions.Load())
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

// (c) Absent from one complete scan → kept; from two → evicted. Reappearing
// resets the count.
func TestISPDispatch_C_EvictAfterTwoCompleteAbsences(t *testing.T) {
	p, mock := newDispatchPool(t)
	p.ispLastBulkRefresh = time.Now().Add(time.Hour)
	before := readDispCounters()
	states := map[string]*ispCampaignState{
		"camp-a": tracked("camp-a", map[string]int{"gmail": 5}),
		"camp-z": tracked("camp-z", map[string]int{"gmail": 5}),
	}
	tick := func(ids ...string) {
		mock.ExpectQuery(reDispScan).WillReturnRows(scanRows(ids...))
		mock.ExpectQuery(reDispOrphan).WillReturnRows(orphanRows())
		p.refreshISPCampaigns(states, map[string]time.Time{})
	}

	tick("camp-a")
	require.Contains(t, states, "camp-z", "one complete absence must not evict")
	tick("camp-a", "camp-z")
	require.Equal(t, 0, states["camp-z"].missedScans, "presence resets the miss count")
	tick("camp-a")
	require.Contains(t, states, "camp-z")
	tick("camp-a")
	require.NotContains(t, states, "camp-z", "two consecutive complete absences evict")
	require.Contains(t, states, "camp-a")
	require.Equal(t, before.evict+1, dispEvictions.Load())
	require.NoError(t, mock.ExpectationsWereMet())
}

// (d) Bulk refresh error → remaining unchanged, lastRefreshOK=false, and the
// campaign is never concluded "fully dispatched" off it. Same for the
// per-campaign read.
func TestISPDispatch_D_RefreshErrorNeverFullyDispatched(t *testing.T) {
	p, mock := newDispatchPool(t)
	before := readDispCounters()
	mock.ExpectQuery(reDispScan).WillReturnRows(scanRows("camp-a"))
	mock.ExpectQuery(reDispBulk).WillReturnError(errDispTimeout)
	mock.ExpectQuery(reDispOrphan).WillReturnRows(orphanRows())

	states := map[string]*ispCampaignState{"camp-a": tracked("camp-a", map[string]int{"gmail": 900})}
	p.refreshISPCampaigns(states, map[string]time.Time{})
	st := states["camp-a"]
	require.Equal(t, 900, st.remaining["gmail"])
	require.False(t, st.lastRefreshOK)
	require.Equal(t, before.scans+1, dispScansFailed.Load())

	st.remaining["gmail"] = 0 // local claims drained it; the DB read failed
	remove, _ := p.dispatchOneCampaign("camp-a", st)
	require.False(t, remove, "'fully dispatched' only from a complete read")
	require.NoError(t, mock.ExpectationsWereMet())

	t.Run("per_campaign_read", func(t *testing.T) {
		p, mock := newDispatchPool(t)
		mock.ExpectQuery(reDispRefresh).WillReturnRows(ispRows("gmail", 5).RowError(0, errDispTimeout))
		st := tracked("c", map[string]int{"gmail": 900})
		require.False(t, p.refreshRemainingFromDB(context.Background(), st))
		require.False(t, st.lastRefreshOK)
		require.Equal(t, 900, st.remaining["gmail"])
	})
}

// (e) Partial GROUP BY (RowError after the first ISP): unseen ISPs keep their
// counts — bulk and per-campaign.
func TestISPDispatch_E_PartialGroupByKeepsUnseenISPs(t *testing.T) {
	p, mock := newDispatchPool(t)
	mock.ExpectQuery(reDispScan).WillReturnRows(scanRows("camp-a"))
	mock.ExpectQuery(reDispBulk).WillReturnRows(sqlmock.NewRows([]string{"c", "isp", "n"}).
		AddRow("camp-a", "gmail", 5).AddRow("camp-a", "yahoo", 9).RowError(1, errDispTimeout))
	mock.ExpectQuery(reDispOrphan).WillReturnRows(orphanRows())
	states := map[string]*ispCampaignState{"camp-a": tracked("camp-a", map[string]int{"gmail": 900, "yahoo": 800})}
	p.refreshISPCampaigns(states, map[string]time.Time{})
	require.Equal(t, map[string]int{"gmail": 900, "yahoo": 800}, states["camp-a"].remaining)
	require.NoError(t, mock.ExpectationsWereMet())

	t.Run("per_campaign_read", func(t *testing.T) {
		p, mock := newDispatchPool(t)
		mock.ExpectQuery(reDispRefresh).WillReturnRows(ispRows("gmail", 5, "yahoo", 9).RowError(1, errDispTimeout))
		st := tracked("c", map[string]int{"gmail": 900, "yahoo": 800})
		p.refreshRemainingFromDB(context.Background(), st)
		require.Equal(t, 800, st.remaining["yahoo"])
		require.Equal(t, 900, st.remaining["gmail"])
	})
}

// Complete bulk read: present ids get fresh counts, a tracked id absent from
// the result has zero queued, and a cooldown campaign whose rows landed is
// re-admitted in the same tick (no 5-min wait).
func TestISPDispatch_BulkCompleteAndCooldownProbe(t *testing.T) {
	p, mock := newDispatchPool(t)
	mock.ExpectQuery(reDispScan).WillReturnRows(scanRows("camp-a", "camp-b", "camp-c"))
	mock.ExpectQuery(reDispBulk).WillReturnRows(sqlmock.NewRows([]string{"c", "isp", "n"}).
		AddRow("camp-a", "gmail", 40).AddRow("camp-c", "gmail", 12))
	mock.ExpectQuery(reDispRefresh).WithArgs("camp-c").WillReturnRows(ispRows("gmail", 12))
	mock.ExpectQuery(reDispOrphan).WillReturnRows(orphanRows())

	states := map[string]*ispCampaignState{
		"camp-a": tracked("camp-a", map[string]int{"gmail": 900}),
		"camp-b": tracked("camp-b", map[string]int{"gmail": 900}),
	}
	cooldown := map[string]time.Time{"camp-c": time.Now()}
	p.refreshISPCampaigns(states, cooldown)
	require.Equal(t, 40, states["camp-a"].remaining["gmail"])
	require.Equal(t, 0, states["camp-b"].remaining["gmail"], "absent from a complete result = zero queued")
	require.True(t, states["camp-b"].lastRefreshOK)
	require.NotContains(t, cooldown, "camp-c")
	require.Contains(t, states, "camp-c")
	require.NoError(t, mock.ExpectationsWereMet())
}

// Zero-queued volume-0 campaigns back off 5 min (legacy re-probed every tick).
func TestISPDispatch_ZeroQueuedBackoff(t *testing.T) {
	p, mock := newDispatchPool(t)
	mock.ExpectQuery(reDispScan).WillReturnRows(scanRows("camp-new"))
	mock.ExpectQuery(reDispDerive).WillReturnRows(ispRows())
	mock.ExpectQuery(reDispOrphan).WillReturnRows(orphanRows())
	cooldown := map[string]time.Time{}
	p.refreshISPCampaigns(map[string]*ispCampaignState{}, cooldown)
	require.Contains(t, cooldown, "camp-new")
	require.Equal(t, 5*time.Minute, p.cooldownTTL())

	// Next tick: on cooldown → no derive query.
	mock.ExpectQuery(reDispScan).WillReturnRows(scanRows("camp-new"))
	mock.ExpectQuery(reDispOrphan).WillReturnRows(orphanRows())
	p.refreshISPCampaigns(map[string]*ispCampaignState{}, cooldown)
	require.NoError(t, mock.ExpectationsWereMet())
}

// (f) The refresh fails (and is slow) → the claim query is still issued in
// the same tick from last-known-good remaining, within the refresh budget.
func TestISPDispatch_F_ClaimIssuedWhenRefreshFails(t *testing.T) {
	for name, bulk := range map[string]func(*sqlmock.ExpectedQuery){
		"partial": func(e *sqlmock.ExpectedQuery) {
			e.WillReturnRows(sqlmock.NewRows([]string{"c", "isp", "n"}).AddRow("camp-a", "gmail", 1).RowError(0, errDispTimeout))
		},
		"slow": func(e *sqlmock.ExpectedQuery) {
			e.WillDelayFor(3 * time.Second).WillReturnRows(sqlmock.NewRows([]string{"c", "isp", "n"}))
		},
	} {
		t.Run(name, func(t *testing.T) {
			p, mock := newDispatchPool(t)
			p.ispBulkTimeout = 100 * time.Millisecond
			mock.ExpectQuery(reDispScan).WillReturnRows(scanRows("camp-a"))
			bulk(mock.ExpectQuery(reDispBulk))
			mock.ExpectQuery(reDispOrphan).WillReturnRows(orphanRows())
			mock.ExpectQuery(reDispClaim).WillReturnRows(sqlmock.NewRows([]string{"id"}))

			states := map[string]*ispCampaignState{"camp-a": tracked("camp-a", map[string]int{"gmail": 500})}
			start := time.Now()
			p.ispDispatchTick(states, map[string]time.Time{})
			require.Less(t, time.Since(start), time.Second)
			require.NoError(t, mock.ExpectationsWereMet(), "claim must be issued in the tick the refresh failed")
			require.Contains(t, states, "camp-a")
		})
	}
}

// Claim errors are counted (and logged rate-limited), no longer silent.
func TestISPDispatch_ClaimErrorsCounted(t *testing.T) {
	p, mock := newDispatchPool(t)
	mock.ExpectQuery(reDispClaim).WillReturnError(errDispTimeout)
	before := readDispCounters()
	items, err := p.claimISPBatch("camp-a", map[string]int{"gmail": 10})
	require.NoError(t, err)
	require.Empty(t, items)
	require.Equal(t, before.claim+1, dispClaimErrors.Load())
}

// (g) Kill switches restore the legacy paths.
func TestISPDispatch_G_KillSwitches(t *testing.T) {
	t.Run("env_parse", func(t *testing.T) {
		t.Setenv("ISP_DISPATCH_LKG", "")
		t.Setenv("ISP_DISPATCH_BULK_REFRESH", "")
		l, s := ispKillSwitchesFromEnv()
		require.False(t, l)
		require.False(t, s)
		t.Setenv("ISP_DISPATCH_LKG", "OFF")
		t.Setenv("ISP_DISPATCH_BULK_REFRESH", "off")
		l, s = ispKillSwitchesFromEnv()
		require.True(t, l)
		require.True(t, s)
	})

	t.Run("lkg_off_evicts_on_first_complete_absence", func(t *testing.T) {
		p, mock := newDispatchPool(t)
		p.ispLegacyEviction = true
		p.ispLastBulkRefresh = time.Now()
		mock.ExpectQuery(reDispScan).WillReturnRows(scanRows("camp-a"))
		mock.ExpectQuery(reDispOrphan).WillReturnRows(orphanRows())
		states := map[string]*ispCampaignState{
			"camp-a": tracked("camp-a", map[string]int{"gmail": 5}),
			"camp-z": tracked("camp-z", map[string]int{"gmail": 5}),
		}
		p.refreshISPCampaigns(states, map[string]time.Time{})
		require.NotContains(t, states, "camp-z")
		require.Equal(t, 60*time.Second, p.cooldownTTL())
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("bulk_off_refreshes_per_campaign", func(t *testing.T) {
		p, mock := newDispatchPool(t)
		p.ispSerialRefresh = true
		mock.ExpectQuery(reDispScan).WillReturnRows(scanRows("camp-a"))
		mock.ExpectQuery(reDispRefresh).WithArgs("camp-a").WillReturnRows(ispRows("gmail", 33))
		mock.ExpectQuery(reDispOrphan).WillReturnRows(orphanRows())
		st := tracked("camp-a", map[string]int{"gmail": 5})
		st.lastRefreshAt = time.Now().Add(-time.Minute)
		states := map[string]*ispCampaignState{"camp-a": st}
		p.refreshISPCampaigns(states, map[string]time.Time{})
		require.Equal(t, 33, st.remaining["gmail"])
		require.NoError(t, mock.ExpectationsWereMet(), "per-campaign query issued, no bulk query")
	})
}

// The dispatcher counters reach the /health send_liveness JSON.
func TestISPDispatch_CountersOnSendLiveness(t *testing.T) {
	dispTracked.Store(7)
	dispRefreshMs.Store(42)
	b, err := json.Marshal(CurrentSendLiveness())
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(b, &m))
	for _, k := range []string{"dispatcher_tracked", "refresh_ms_last", "admit_count_ms_last", "scans_failed", "evictions", "admit_failed", "claim_errors", "stats_guard_analyzes", "stats_guard_errors", "stats_guard_last_at"} {
		require.Contains(t, m, k)
	}
	require.EqualValues(t, 7, m["dispatcher_tracked"])
	require.EqualValues(t, 42, m["refresh_ms_last"])
}
