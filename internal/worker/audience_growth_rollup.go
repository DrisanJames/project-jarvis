package worker

// Audience growth rollup — one row per (org × Denver day × ISP) in
// mailing_audience_growth_daily, read by GET /api/mailing/dashboard/audience-growth.
//
// WHY A ROLLUP: acquired is a heap read over ~2.7M mailing_subscribers rows for
// 7 days, and ISP needs the email on every one of them. Measured 2026-11-02:
// 45s for the bare count against the dashboard's 8s budget, so the panel
// returned 500 on every load.
//
// SEMANTICS match the old live query. acquired = rows created that day.
// churned = rows in a terminal status whose updated_at falls on that day.
// ISP comes from the address through growthISPCase, never the isp column
// (polluted with PMTA *.queue names).
//
// Runs inside GrowthRollupWorker's pass and lock, so it shares its 2h cadence,
// no-boot-pass rule and DISABLE_GROWTH_ROLLUP kill switch. Re-run safe: a day
// is replaced whole in one transaction, so a reader never sees it half-written.

import (
	"context"
	"fmt"
	"log"
	"time"
)

// audienceGrowthWindowDays is the span the dashboard reads; missing days in it
// are filled on the next pass.
const audienceGrowthWindowDays = 7

var audienceGrowthInsertSQL = fmt.Sprintf(`
	INSERT INTO mailing_audience_growth_daily (organization_id, day, isp, acquired, churned)
	SELECT $4::uuid,
	       $3::date,
	       COALESCE(a.isp, c.isp),
	       COALESCE(a.n, 0),
	       COALESCE(c.n, 0)
	FROM (
		SELECT %[1]s AS isp, COUNT(*) AS n
		FROM mailing_subscribers
		WHERE organization_id = $4
		  AND created_at >= $1 AND created_at < $2
		GROUP BY 1
	) a
	FULL JOIN (
		SELECT %[1]s AS isp, COUNT(*) AS n
		FROM mailing_subscribers
		WHERE organization_id = $4
		  AND status IN ('bounced','complained','blacklisted','unsubscribed')
		  AND updated_at >= $1 AND updated_at < $2
		GROUP BY 1
	) c ON c.isp = a.isp
`, growthISPCase("split_part(email, '@', 2)"))

// audienceDaysToCompute returns the trailing days plus any day of the dashboard
// window that has no row yet. A churn row's updated_at can move, so only the
// trailing days are worth recomputing on every pass.
func (w *GrowthRollupWorker) audienceDaysToCompute(ctx context.Context, now time.Time) ([]time.Time, error) {
	today, _, _ := denverDayBounds(now, w.loc)
	floor := today.AddDate(0, 0, -(audienceGrowthWindowDays - 1))

	rows, err := w.db.QueryContext(ctx, `
		SELECT DISTINCT day FROM mailing_audience_growth_daily WHERE day >= $1::date
	`, floor.Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	covered := map[string]bool{}
	for rows.Next() {
		var d time.Time
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		covered[d.Format("2006-01-02")] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var days []time.Time
	for i := 0; i < audienceGrowthWindowDays; i++ {
		d := today.AddDate(0, 0, -i)
		if i < w.trailingDays || !covered[d.Format("2006-01-02")] {
			days = append(days, d)
		}
	}
	return days, nil
}

// audienceGrowthOrgs lists the orgs to roll up. Each is bound as a literal
// organization_id: measured 2026-11-02, one day took 0.6s that way and 50s
// through an IN (SELECT ...), which loses the (organization_id, created_at) index.
func (w *GrowthRollupWorker) audienceGrowthOrgs(ctx context.Context) ([]string, error) {
	rows, err := w.db.QueryContext(ctx, laneStatsRollupOrgsSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var orgs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		orgs = append(orgs, id)
	}
	return orgs, rows.Err()
}

// computeAudienceDay replaces one org's rows for one Denver day.
func (w *GrowthRollupWorker) computeAudienceDay(ctx context.Context, orgID string, day time.Time) error {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	tx, err := w.db.BeginTx(cctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck — no-op after Commit
	if _, err := tx.ExecContext(cctx, "SET LOCAL statement_timeout = '"+growthStatementTimeout+"'"); err != nil {
		return err
	}
	dayStr := day.Format("2006-01-02")
	if _, err := tx.ExecContext(cctx, `DELETE FROM mailing_audience_growth_daily WHERE day = $1::date AND organization_id = $2`, dayStr, orgID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(cctx, audienceGrowthInsertSQL, day, day.AddDate(0, 0, 1), dayStr, orgID); err != nil {
		return err
	}
	return tx.Commit()
}

// runAudienceGrowth computes every due day and returns how many failed.
func (w *GrowthRollupWorker) runAudienceGrowth(ctx context.Context, now time.Time) (done, failed int) {
	days, err := w.audienceDaysToCompute(ctx, now)
	if err != nil {
		log.Printf("[GrowthRollup] audience coverage scan: %v", err)
		return 0, 1
	}
	orgs, err := w.audienceGrowthOrgs(ctx)
	if err != nil {
		log.Printf("[GrowthRollup] audience org list: %v", err)
		return 0, 1
	}
	for _, d := range days {
		if ctx.Err() != nil {
			return done, failed
		}
		for _, org := range orgs {
			if err := w.computeAudienceDay(ctx, org, d); err != nil {
				failed++
				log.Printf("[GrowthRollup] audience day %s org %s: %v", d.Format("2006-01-02"), org, err)
			} else {
				done++
			}
		}
		select {
		case <-ctx.Done():
			return done, failed
		case <-time.After(w.chunkPause):
		}
	}
	return done, failed
}
