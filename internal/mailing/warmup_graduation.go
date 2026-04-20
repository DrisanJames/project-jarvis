package mailing

// Per-domain warmup graduation.
//
// The SDS `warmup_status` column has four states, stamped with
// `warmup_status_changed_at` on every transition so we can audit the
// lifecycle later:
//
//   cold      - never mailed on this domain. Included as pure prospect
//               volume, no engagement signal yet.
//   warming   - has been mailed at least once. Kept in the audience
//               while we accumulate signal, but filtered to recent
//               opens once we have enough sends under our belt.
//   engaged   - clear positive signal: total_sent>=3, total_opens>=2,
//               last_open within 60 days. These are our best recipients.
//   dormant   - used to be engaged, went quiet for 120+ days. Frozen
//               out until they re-open or we trim them.
//
// The transitions run as a nightly job:
//
//   cold     → warming   (handled inline by UpsertSDSSend in
//                         subscriber_domain_state.go — the first send
//                         already bumps status to 'warming')
//   warming  → engaged   (this job; criteria above)
//   engaged  → dormant   (this job; no open in 120 days)
//
// dormant → warming is intentionally NOT automatic. If a dormant
// recipient opens again it stays dormant in SDS but the per-domain
// `last_open_at` still updates; we let a human operator or a targeted
// re-engagement campaign explicitly reset the state.
//
// Always stamp `warmup_status_changed_at` so the history of transitions
// is reconstructable from the column alone — crucial when diagnosing
// audience-shape regressions months later.

import (
	"context"
	"database/sql"
	"log"
	"sync"
	"time"
)

// WarmupGraduator runs the nightly warming→engaged and engaged→dormant
// sweeps. Safe to run concurrently across multiple server replicas —
// the UPDATE statements are idempotent and race-tolerant.
type WarmupGraduator struct {
	db *sql.DB

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// WarmupGraduationStats is returned from Sweep so operators can see how
// many rows transitioned in a given run. Aggregated across domains.
type WarmupGraduationStats struct {
	WarmingToEngaged int64
	EngagedToDormant int64
	Elapsed          time.Duration
}

// NewWarmupGraduator constructs a graduator tied to the given database.
// Call Start to kick off the nightly loop; Stop to exit cleanly.
func NewWarmupGraduator(db *sql.DB) *WarmupGraduator {
	return &WarmupGraduator{db: db}
}

// Start launches a background goroutine that runs Sweep once per 24h
// (aligned to UTC midnight + 2h). Call Stop on shutdown.
func (g *WarmupGraduator) Start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	g.ctx = ctx
	g.cancel = cancel

	g.wg.Add(1)
	go g.loop()
}

// Stop cancels the loop and waits for the goroutine to drain.
func (g *WarmupGraduator) Stop() {
	if g.cancel != nil {
		g.cancel()
	}
	g.wg.Wait()
}

func (g *WarmupGraduator) loop() {
	defer g.wg.Done()

	// Run once on boot after a 2-minute settling delay so the server
	// is past its startup noise. Then align to 02:00 UTC nightly.
	timer := time.NewTimer(2 * time.Minute)
	defer timer.Stop()

	for {
		select {
		case <-g.ctx.Done():
			return
		case <-timer.C:
			stats, err := g.Sweep(g.ctx)
			if err != nil {
				log.Printf("[warmup] sweep error: %v", err)
			} else {
				log.Printf("[warmup] sweep done: warming→engaged=%d engaged→dormant=%d elapsed=%s",
					stats.WarmingToEngaged, stats.EngagedToDormant, stats.Elapsed)
			}
			timer.Reset(nextSweepDelay(time.Now().UTC()))
		}
	}
}

// nextSweepDelay returns how long until the next 02:00 UTC.
func nextSweepDelay(now time.Time) time.Duration {
	target := time.Date(now.Year(), now.Month(), now.Day(), 2, 0, 0, 0, time.UTC)
	if !target.After(now) {
		target = target.Add(24 * time.Hour)
	}
	return target.Sub(now)
}

// Sweep executes both transitions in a single pass. Errors on one
// transition do not short-circuit the other — we want operators to see
// partial progress rather than nothing. Returns aggregate counts.
func (g *WarmupGraduator) Sweep(ctx context.Context) (WarmupGraduationStats, error) {
	var stats WarmupGraduationStats
	t0 := time.Now()

	n1, err1 := g.graduateWarmingToEngaged(ctx)
	if err1 != nil {
		log.Printf("[warmup] warming→engaged error: %v", err1)
	}
	stats.WarmingToEngaged = n1

	n2, err2 := g.demoteEngagedToDormant(ctx)
	if err2 != nil {
		log.Printf("[warmup] engaged→dormant error: %v", err2)
	}
	stats.EngagedToDormant = n2

	stats.Elapsed = time.Since(t0)
	if err1 != nil {
		return stats, err1
	}
	return stats, err2
}

// graduateWarmingToEngaged promotes any (subscriber, domain) that has
// at least 3 sends, 2 opens, and an open within the last 60 days.
// warmup_status_changed_at is always updated — even for rows that were
// touched on a prior sweep, the condition re-evaluates freshly.
func (g *WarmupGraduator) graduateWarmingToEngaged(ctx context.Context) (int64, error) {
	res, err := g.db.ExecContext(ctx, `
		UPDATE mailing_subscriber_domain_state
		SET warmup_status = 'engaged',
		    warmup_status_changed_at = NOW(),
		    updated_at = NOW()
		WHERE warmup_status = 'warming'
		  AND total_sent >= 3
		  AND total_opens >= 2
		  AND last_open_at IS NOT NULL
		  AND last_open_at > NOW() - INTERVAL '60 days'
	`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// demoteEngagedToDormant freezes any engaged row whose last_open_at is
// older than 120 days. A subsequent open will update last_open_at but
// intentionally does not reverse the demotion — dormant→warming is an
// explicit operator action.
func (g *WarmupGraduator) demoteEngagedToDormant(ctx context.Context) (int64, error) {
	res, err := g.db.ExecContext(ctx, `
		UPDATE mailing_subscriber_domain_state
		SET warmup_status = 'dormant',
		    warmup_status_changed_at = NOW(),
		    updated_at = NOW()
		WHERE warmup_status = 'engaged'
		  AND (last_open_at IS NULL OR last_open_at < NOW() - INTERVAL '120 days')
	`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
