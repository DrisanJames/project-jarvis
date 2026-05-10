package worker

// SDSGraduationJob is the nightly cleanup worker for the per-domain
// engagement engine described in
// .scratch/PER_DOMAIN_ENGAGEMENT_ENGINE_SPEC.md (SA-1).
//
// Two passes, in order, once per day at 02:00 UTC:
//
//  1. Cold pass: any mailing_subscriber_domain_state row with
//     total_sent >= 4, no opens, no clicks, and state != 'cold' is
//     promoted to 'cold'. The inline UpsertSDSSend writer covers this
//     transition on the live send path; this pass is a backstop for
//     race-pinched writes (e.g. a send that incremented total_sent
//     concurrently with another transaction that landed first and
//     missed the threshold check).
//
//  2. Cross-engaged pass: any subscriber with state='engaged' on >= 2
//     distinct sending_domains is marked cross_engaged on
//     mailing_subscribers. The audience finalizer (SA-2) prioritizes
//     cross_engaged subscribers across all brand sends because they
//     have proven inbox-placement signal across the portfolio. Setting
//     cross_engaged is one-way for now: once a subscriber graduates,
//     they stay graduated even if they later fall back to probe on
//     the original two domains. A future demotion pass can revisit
//     this; intentionally out of scope for SA-1.
//
// EXPLICITLY OMITTED per operator decision 2026-05-09: there is NO
// cold-decay re-probe pass. Cold rows stay cold until natural future
// engagement (open or click) re-promotes them via the inline
// UpsertSDSOpen / UpsertSDSClick writers. We do not periodically
// re-attempt cold subscribers.
//
// Lifecycle: NewSDSGraduationJob(db) -> Start() -> Stop(). Start spawns
// one goroutine that sleeps until the next 02:00 UTC tick and then
// runs daily. Stop cancels the context, signals the goroutine, and
// waits for it to exit. Both methods are idempotent.

import (
	"context"
	"database/sql"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultSDSGraduationInterval is the standing daily cadence. Exposed
// as a constant so tests can override via the WithInterval helper.
const DefaultSDSGraduationInterval = 24 * time.Hour

// SDSGraduationJob nightly cleanup + cross-engaged graduation.
type SDSGraduationJob struct {
	db       *sql.DB
	interval time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	running atomic.Bool
}

// NewSDSGraduationJob constructs the job with sensible defaults.
// Caller must invoke Start() to spawn the goroutine.
func NewSDSGraduationJob(db *sql.DB) *SDSGraduationJob {
	ctx, cancel := context.WithCancel(context.Background())
	return &SDSGraduationJob{
		db:       db,
		interval: DefaultSDSGraduationInterval,
		ctx:      ctx,
		cancel:   cancel,
	}
}

// WithInterval overrides the daily cadence. Tests use this to drive
// the loop without waiting 24h. Must be called before Start.
func (j *SDSGraduationJob) WithInterval(d time.Duration) *SDSGraduationJob {
	if d > 0 {
		j.interval = d
	}
	return j
}

// Start spawns the daily goroutine. Idempotent: subsequent calls
// while the job is running are no-ops. Returns an error only if the
// db handle is nil — callers should treat startup failure as fatal.
func (j *SDSGraduationJob) Start() error {
	if j.db == nil {
		return ErrSDSGraduationNoDB
	}
	if !j.running.CompareAndSwap(false, true) {
		return nil
	}
	j.wg.Add(1)
	go j.loop()
	log.Printf("[SDSGraduationJob] started (interval=%s, first run at next 02:00 UTC)", j.interval)
	return nil
}

// Stop signals the goroutine to exit and waits for it. Idempotent.
func (j *SDSGraduationJob) Stop() {
	if !j.running.CompareAndSwap(true, false) {
		return
	}
	j.cancel()
	j.wg.Wait()
	log.Println("[SDSGraduationJob] stopped")
}

// ErrSDSGraduationNoDB is returned by Start when no DB handle is set.
var ErrSDSGraduationNoDB = sdsGraduationError("nil db handle")

type sdsGraduationError string

func (e sdsGraduationError) Error() string { return "sds_graduation: " + string(e) }

// loop is the daily ticker. First fires at the next 02:00 UTC after
// Start, then every j.interval. Exits on ctx cancellation.
func (j *SDSGraduationJob) loop() {
	defer j.wg.Done()

	timer := time.NewTimer(j.timeUntilNextTick(time.Now().UTC()))
	defer timer.Stop()

	for {
		select {
		case <-j.ctx.Done():
			return
		case <-timer.C:
			j.runOnce(j.ctx)
			timer.Reset(j.interval)
		}
	}
}

// timeUntilNextTick computes how long to sleep until the next 02:00 UTC.
// If we're already past 02:00 today, schedule for 02:00 tomorrow.
// Exposed as a method (not inlined) so the test for the daily anchor
// is straightforward to write later if needed.
func (j *SDSGraduationJob) timeUntilNextTick(now time.Time) time.Duration {
	target := time.Date(now.Year(), now.Month(), now.Day(), 2, 0, 0, 0, time.UTC)
	if !now.Before(target) {
		target = target.Add(24 * time.Hour)
	}
	return target.Sub(now)
}

// RunOnce executes the cold pass and cross-engaged pass back-to-back.
// Exported so tests and operational tooling can trigger the work
// without waiting for the daily tick. The loop calls the unexported
// runOnce which is a thin wrapper for symmetry.
func (j *SDSGraduationJob) RunOnce(ctx context.Context) {
	j.runOnce(ctx)
}

func (j *SDSGraduationJob) runOnce(ctx context.Context) {
	if err := ctx.Err(); err != nil {
		return
	}
	j.runColdPass(ctx)
	if err := ctx.Err(); err != nil {
		return
	}
	j.runCrossEngagedPass(ctx)
}

// runColdPass flips any not-yet-cold SDS row that has crossed the
// 4-send / zero-engagement threshold to state='cold'. The inline
// UpsertSDSSend writer handles this transition on the hot path; this
// pass is a backstop for race-pinched writes. Engaged rows are
// untouched: total_opens > 0 OR total_clicks > 0 short-circuits the
// WHERE clause, and the explicit `state != 'cold'` filter prevents
// re-stamping state_updated_at on rows that are already cold.
//
// IMPORTANT (negative invariant): this UPDATE never demotes engaged
// subscribers. There is NO cold-decay re-probe pass anywhere in this
// job — confirmed by TestSDSGraduationJob_NoColdDecayPass.
func (j *SDSGraduationJob) runColdPass(ctx context.Context) {
	res, err := j.db.ExecContext(ctx, `
		UPDATE mailing_subscriber_domain_state
		SET state = 'cold', state_updated_at = NOW(), updated_at = NOW()
		WHERE total_sent >= 4
		  AND total_opens = 0
		  AND total_clicks = 0
		  AND state != 'cold'
	`)
	if err != nil {
		log.Printf("[SDSGraduationJob] cold pass failed: %v", err)
		return
	}
	if rows, err := res.RowsAffected(); err == nil {
		log.Printf("[SDSGraduationJob] cold pass: %d rows transitioned to state='cold'", rows)
	}
}

// runCrossEngagedPass marks subscribers as cross_engaged when they
// have state='engaged' on >= 2 distinct sending_domains. Flag is
// one-way for now (no demotion) — this matches the SA-2 finalizer
// contract that uses cross_engaged as a sticky priority signal.
func (j *SDSGraduationJob) runCrossEngagedPass(ctx context.Context) {
	res, err := j.db.ExecContext(ctx, `
		UPDATE mailing_subscribers
		SET cross_engaged = true,
		    cross_engaged_at = NOW(),
		    updated_at = NOW()
		WHERE id IN (
			SELECT subscriber_id
			FROM mailing_subscriber_domain_state
			WHERE state = 'engaged'
			GROUP BY subscriber_id
			HAVING COUNT(DISTINCT sending_domain) >= 2
		)
		AND (cross_engaged IS NULL OR cross_engaged = false)
	`)
	if err != nil {
		log.Printf("[SDSGraduationJob] cross-engaged pass failed: %v", err)
		return
	}
	if rows, err := res.RowsAffected(); err == nil {
		log.Printf("[SDSGraduationJob] cross-engaged pass: %d subscribers graduated", rows)
	}
}
