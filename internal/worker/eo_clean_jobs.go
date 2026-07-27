package worker

// EOCleanJobWorker — the EO CLEANING SERVICE drain half (operator ask
// 2026-07-26: "Somewhere in the platform I should be able to upload a list
// and have it cleaned. Or tell the platform to clean a particular list or
// segment from the segment command.").
//
// Job intake (internal/api/eo_clean_handlers.go) lands ad-hoc cleaning jobs in
// mailing_eo_clean_jobs + mailing_eo_clean_items (emails already 'Verified' in
// mailing_eo_validation are skipped at enqueue — never pay twice). This worker
// drains pending items across running jobs FIFO (oldest job first), calls the
// SAME EO transport+classification as the PartnerValidator (callEOValidation,
// partner_validator.go — one EO client path, one outcome vocabulary), and
// lands every terminal verdict in BOTH:
//   - mailing_eo_validation — the canonical gate every fresh draw reads.
//     Newest-wins upsert, the EXACT ON CONFLICT semantics of
//     agents/scheduling/eo_tranche_landing.py upsert_eo_validation;
//     source_file = 'eo-clean-job/<job-id>'.
//   - the mailing_eo_clean_items row (per-job progress truth).
//
// COST CONTROLS (cleaning costs money — the operator said so):
//   - global daily budget: env EO_CLEAN_DAILY_BUDGET (default 100000 EO calls
//     per UTC day across ALL jobs), recomputed each pass from items validated
//     today — restart-safe, no in-memory ledger. (Retry calls that stay
//     pending are budget-counted within the pass but not persisted; the
//     recomputation therefore slightly undercounts across restarts.)
//   - per-job daily_cap (0 = uncapped), same recomputation per job.
//   - no boot burst: the first drain happens one PollInterval after boot
//     (ticker only — the 2026-07-13 send-hours-boot-pass lesson), and each
//     pass processes at most BatchSize items.
//
// Verdict → counter mapping (classifyCleanOutcome): Verified → verified,
// Complainer → complainer (standing doctrine: Complainer IS mailable),
// Undeliverable → undeliverable, everything else terminal (SpamTrap / Bot /
// Seed Account / Catch All / Role / account-suppression REJECT → 'Suppressed')
// → other. Retryable outcomes (EO ResultID 0/11, transport errors) stay
// pending up to MaxRetries attempts, then the item goes 'failed' WITHOUT a
// mailing_eo_validation write (no verdict was obtained — never invent one).
//
// Idioms: single-writer distlock per pass (Redis preferred, PG advisory
// fallback — engagement_family_builder.go contract); heartbeat 'eo_clean_jobs'
// in mailing_worker_heartbeats every pass; RecordWorkerRun only for passes
// that did work. Kill switch: DISABLE_EO_CLEAN_JOBS=true. Restart/double-fire
// safe: claims are plain SELECTs under the distlock, item updates are keyed
// (job_id, email_lower), the eo_validation upsert is newest-wins, and a crash
// mid-batch leaves items 'pending' for the next pass (re-validated — costs one
// duplicate call, corrupts nothing).

import (
	"context"
	"database/sql"
	"log"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/emailoversight"
	"github.com/ignite/sparkpost-monitor/internal/pkg/distlock"
	"github.com/redis/go-redis/v9"
)

const (
	eoCleanWorkerName = "eo_clean_jobs"
	eoCleanLockKey    = "eo-clean-jobs"

	// eoCleanDefaultDailyBudget is the default global EO-call budget per UTC
	// day across all clean jobs (env EO_CLEAN_DAILY_BUDGET overrides).
	eoCleanDefaultDailyBudget = 100000

	// eoCleanSourceFilePrefix stamps mailing_eo_validation.source_file so a
	// verdict's provenance is queryable ('eo-clean-job/<job-id>').
	eoCleanSourceFilePrefix = "eo-clean-job/"
)

// EOCleanJobConfig knobs (zero values take the defaults below — the
// PartnerValidatorConfig idiom).
type EOCleanJobConfig struct {
	BatchSize    int           // max items per pass across all jobs (default 200)
	PollInterval time.Duration // default 30s
	Concurrency  int           // EO calls in parallel (default 10)
	MaxRetries   int           // attempts before an item goes 'failed' (default 3)
	DailyBudget  int           // global EO calls/day; <=0 reads EO_CLEAN_DAILY_BUDGET (default 100000)
}

// EOCleanJobWorker drains mailing_eo_clean_items for queued/running jobs.
type EOCleanJobWorker struct {
	db       *sql.DB
	redis    *redis.Client
	eoClient EOValidator
	cfg      EOCleanJobConfig
}

// NewEOCleanJobWorker constructs the worker. redisClient may be nil — the
// distlock falls back to a PG advisory lock (same contract as siblings).
func NewEOCleanJobWorker(db *sql.DB, redisClient *redis.Client, eoClient EOValidator, cfg EOCleanJobConfig) *EOCleanJobWorker {
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 200
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 30 * time.Second
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 10
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 3
	}
	if cfg.DailyBudget <= 0 {
		cfg.DailyBudget = eoCleanDailyBudgetFromEnv()
	}
	return &EOCleanJobWorker{db: db, redis: redisClient, eoClient: eoClient, cfg: cfg}
}

// eoCleanDailyBudgetFromEnv reads EO_CLEAN_DAILY_BUDGET (default 100000).
func eoCleanDailyBudgetFromEnv() int {
	if raw := os.Getenv("EO_CLEAN_DAILY_BUDGET"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return n
		}
		log.Printf("[EOCleanJobs] invalid EO_CLEAN_DAILY_BUDGET=%q — using default %d", raw, eoCleanDefaultDailyBudget)
	}
	return eoCleanDefaultDailyBudget
}

// Start runs the poll loop until ctx is cancelled; call via `go w.Start(ctx)`.
// NO boot burst: the first drain happens one PollInterval after boot.
func (w *EOCleanJobWorker) Start(ctx context.Context) {
	if os.Getenv("DISABLE_EO_CLEAN_JOBS") == "true" {
		log.Printf("[EOCleanJobs] disabled via DISABLE_EO_CLEAN_JOBS")
		return
	}
	if w.db == nil || w.eoClient == nil {
		log.Printf("[EOCleanJobs] disabled (db or EO client missing)")
		return
	}
	log.Printf("[EOCleanJobs] started — batch=%d poll=%s concurrency=%d daily_budget=%d (kill: DISABLE_EO_CLEAN_JOBS)",
		w.cfg.BatchSize, w.cfg.PollInterval, w.cfg.Concurrency, w.cfg.DailyBudget)
	t := time.NewTicker(w.cfg.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("[EOCleanJobs] stopping")
			return
		case <-t.C:
			w.RunOnce(ctx)
		}
	}
}

// RunOnce executes one drain pass under the distributed lock. Exported so
// tests and operational tooling can trigger a pass directly.
func (w *EOCleanJobWorker) RunOnce(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	lock := distlock.NewLock(w.redis, w.db, eoCleanLockKey, 15*time.Minute)
	acquired, err := lock.Acquire(ctx)
	if err != nil {
		log.Printf("[EOCleanJobs] lock acquire error: %v", err)
		return
	}
	if !acquired {
		return // another instance holds the lock — quiet skip (30s cadence)
	}
	defer func() {
		if err := lock.Release(context.Background()); err != nil {
			log.Printf("[EOCleanJobs] lock release error: %v", err)
		}
	}()

	start := time.Now()
	processed, failed, passErr := w.drain(ctx)
	hbStatus, hbErr := "ok", ""
	if passErr != nil {
		hbStatus, hbErr = "error", passErr.Error()
	}
	EmitHeartbeat(ctx, w.db, eoCleanWorkerName, int(w.cfg.PollInterval.Seconds()), hbStatus, hbErr)
	if processed > 0 || failed > 0 || passErr != nil {
		runStatus := "ok"
		if passErr != nil {
			runStatus = "failed"
		} else if failed > 0 {
			runStatus = "partial"
		}
		RecordWorkerRun(ctx, w.db, eoCleanWorkerName, start, runStatus, processed, failed,
			"ad-hoc EO cleaning jobs (mailing_eo_clean_jobs)")
	}
}

// eoCleanJobRow is one drainable job.
type eoCleanJobRow struct {
	id       string
	dailyCap int
	status   string
}

// drain processes up to BatchSize pending items across drainable jobs, FIFO by
// job creation. Returns (terminal-verdict items, failed items, first error).
func (w *EOCleanJobWorker) drain(ctx context.Context) (int, int, error) {
	spent, err := w.globalSpentToday(ctx)
	if err != nil {
		return 0, 0, err
	}
	budgetLeft := w.cfg.DailyBudget - spent
	if budgetLeft <= 0 {
		return 0, 0, nil // daily budget exhausted — resume tomorrow
	}

	jobs, err := w.drainableJobs(ctx)
	if err != nil {
		return 0, 0, err
	}
	batchLeft := w.cfg.BatchSize
	var processed, failedN int
	for _, job := range jobs {
		if ctx.Err() != nil || batchLeft <= 0 || budgetLeft <= 0 {
			break
		}
		jobSpent := 0
		if job.dailyCap > 0 {
			jobSpent, err = w.jobSpentToday(ctx, job.id)
			if err != nil {
				return processed, failedN, err
			}
		}
		n := eoCleanClaimLimit(batchLeft, budgetLeft, job.dailyCap, jobSpent)
		if n <= 0 {
			continue // this job's daily cap is exhausted — next job
		}
		items, err := w.claimItems(ctx, job.id, n)
		if err != nil {
			return processed, failedN, err
		}
		if len(items) == 0 {
			w.maybeFinishJob(ctx, job.id)
			continue
		}
		if job.status == "queued" {
			w.markJobRunning(ctx, job.id)
		}
		done, fails := w.validateAndApplyClean(ctx, job.id, items)
		processed += done
		failedN += fails
		batchLeft -= len(items)
		budgetLeft -= len(items) // every claim = one EO call this pass
		if done+fails > 0 && done+fails == len(items) {
			// Batch may have exhausted the job — check cheaply.
			w.maybeFinishJob(ctx, job.id)
		}
	}
	return processed, failedN, nil
}

// eoCleanClaimLimit computes how many items may be claimed for one job:
// min(batchLeft, budgetLeft, per-job cap remaining). dailyCap<=0 = uncapped.
// Never negative.
func eoCleanClaimLimit(batchLeft, budgetLeft, dailyCap, spentToday int) int {
	n := batchLeft
	if budgetLeft < n {
		n = budgetLeft
	}
	if dailyCap > 0 {
		capLeft := dailyCap - spentToday
		if capLeft < n {
			n = capLeft
		}
	}
	if n < 0 {
		return 0
	}
	return n
}

// globalSpentToday counts EO calls persisted today (UTC day) across all jobs —
// items that reached a terminal state stamp validated_at.
func (w *EOCleanJobWorker) globalSpentToday(ctx context.Context) (int, error) {
	qctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var n int
	err := w.db.QueryRowContext(qctx, `
		SELECT COUNT(*) FROM mailing_eo_clean_items
		WHERE validated_at >= date_trunc('day', NOW())
	`).Scan(&n)
	return n, err
}

// jobSpentToday counts one job's persisted EO calls today.
func (w *EOCleanJobWorker) jobSpentToday(ctx context.Context, jobID string) (int, error) {
	qctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var n int
	err := w.db.QueryRowContext(qctx, `
		SELECT COUNT(*) FROM mailing_eo_clean_items
		WHERE job_id = $1::uuid AND validated_at >= date_trunc('day', NOW())
	`, jobID).Scan(&n)
	return n, err
}

// drainableJobs lists queued/running jobs FIFO. Paused/done/failed jobs are
// never touched — pause takes effect at the next claim boundary.
func (w *EOCleanJobWorker) drainableJobs(ctx context.Context) ([]eoCleanJobRow, error) {
	qctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	rows, err := w.db.QueryContext(qctx, `
		SELECT id::text, daily_cap, status
		FROM mailing_eo_clean_jobs
		WHERE status IN ('queued', 'running')
		ORDER BY created_at ASC
		LIMIT 20
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]eoCleanJobRow, 0, 8)
	for rows.Next() {
		var j eoCleanJobRow
		if err := rows.Scan(&j.id, &j.dailyCap, &j.status); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// eoCleanItem is one claimed pending item.
type eoCleanItem struct {
	emailLower string
	attempts   int
}

// claimItems reads up to n pending items for one job. Plain SELECT — the
// pass-level distlock is the single-writer guarantee (no FOR UPDATE needed;
// a crash between claim and apply leaves the row 'pending' for the next pass).
func (w *EOCleanJobWorker) claimItems(ctx context.Context, jobID string, n int) ([]eoCleanItem, error) {
	qctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	rows, err := w.db.QueryContext(qctx, `
		SELECT email_lower, attempts
		FROM mailing_eo_clean_items
		WHERE job_id = $1::uuid AND status = 'pending'
		ORDER BY email_lower
		LIMIT $2
	`, jobID, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]eoCleanItem, 0, n)
	for rows.Next() {
		var it eoCleanItem
		if err := rows.Scan(&it.emailLower, &it.attempts); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func (w *EOCleanJobWorker) markJobRunning(ctx context.Context, jobID string) {
	if _, err := w.db.ExecContext(ctx, `
		UPDATE mailing_eo_clean_jobs SET status = 'running'
		WHERE id = $1::uuid AND status = 'queued'
	`, jobID); err != nil {
		log.Printf("[EOCleanJobs] mark_running %s: %v", jobID, err)
	}
}

// maybeFinishJob completes a job whose items are fully drained (no pending
// rows left). Guarded so paused jobs are never auto-completed.
func (w *EOCleanJobWorker) maybeFinishJob(ctx context.Context, jobID string) {
	res, err := w.db.ExecContext(ctx, `
		UPDATE mailing_eo_clean_jobs SET status = 'done', finished_at = NOW()
		WHERE id = $1::uuid AND status IN ('queued', 'running')
		  AND NOT EXISTS (
			SELECT 1 FROM mailing_eo_clean_items
			WHERE job_id = $1::uuid AND status = 'pending'
		  )
	`, jobID)
	if err != nil {
		log.Printf("[EOCleanJobs] finish_job %s: %v", jobID, err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("[EOCleanJobs] job %s DONE (all items drained)", jobID)
	}
}

// eoCleanItemAction is the terminal decision for one processed item.
type eoCleanItemAction int

const (
	eoCleanActionRetry eoCleanItemAction = iota // stays pending, attempts+1
	eoCleanActionDone                           // terminal verdict → eo_validation + item done
	eoCleanActionFailed                         // attempts exhausted, NO verdict → item failed
)

// eoCleanDelta accumulates one batch's job-counter changes.
type eoCleanDelta struct {
	validated, verified, complainer, undeliverable, other, failed int
}

// classifyCleanOutcome maps a shared-transport outcome onto the canonical
// mailing_eo_validation status vocabulary + the job counter it increments.
// Terminal=false means retryable (no status). REJECT (account suppression)
// records 'Suppressed' — the eo_clean_inbase_tranches/eo_tranche_landing
// convention. Verified/Complainer derive from ResultID (1/7), never from the
// raw result string.
func classifyCleanOutcome(o eoOutcome) (status, counter string, terminal bool) {
	switch o.kind {
	case outcomeReady:
		if o.resultID == 7 {
			return "Complainer", "complainer", true
		}
		return "Verified", "verified", true
	case outcomeSuppress:
		if o.resultID == emailoversight.ResultIDRejected {
			return "Suppressed", "other", true
		}
		if o.result == "Undeliverable" {
			return "Undeliverable", "undeliverable", true
		}
		st := o.result
		if st == "" {
			st = "Suppressed"
		}
		return st, "other", true
	default: // outcomeRetry, outcomeError
		return "", "", false
	}
}

// applyCleanOutcome folds one item outcome into the batch delta and decides
// the item's action. attempts is the item's PRIOR attempt count; this call
// represents attempt attempts+1 (the PartnerValidator.markRetry cap rule).
func applyCleanOutcome(d *eoCleanDelta, o eoOutcome, attempts, maxRetries int) (eoCleanItemAction, string) {
	status, counter, terminal := classifyCleanOutcome(o)
	if !terminal {
		if attempts+1 >= maxRetries {
			d.failed++
			return eoCleanActionFailed, o.result
		}
		return eoCleanActionRetry, o.result
	}
	d.validated++
	switch counter {
	case "verified":
		d.verified++
	case "complainer":
		d.complainer++
	case "undeliverable":
		d.undeliverable++
	default:
		d.other++
	}
	return eoCleanActionDone, status
}

// validateAndApplyClean runs EO concurrently over one job's claimed items
// (the PartnerValidator.validateAndApply shape: bounded sem + gentle pacing),
// then applies item rows, the eo_validation upsert, and one job-counter
// update. Returns (done, failed).
func (w *EOCleanJobWorker) validateAndApplyClean(ctx context.Context, jobID string, items []eoCleanItem) (int, int) {
	results := make([]eoOutcome, len(items))
	sem := make(chan struct{}, w.cfg.Concurrency)
	var wg sync.WaitGroup
	for i, it := range items {
		wg.Add(1)
		go func(idx int, email string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[idx] = callEOValidation(ctx, w.eoClient, email)
			time.Sleep(150 * time.Millisecond) // gentle pacing for the EO endpoint
		}(i, it.emailLower)
	}
	wg.Wait()

	sourceFile := eoCleanSourceFilePrefix + jobID
	var delta eoCleanDelta
	var done, failedN int
	for i, o := range results {
		it := items[i]
		action, result := applyCleanOutcome(&delta, o, it.attempts, w.cfg.MaxRetries)
		switch action {
		case eoCleanActionDone:
			if err := w.upsertEOValidation(ctx, it.emailLower, result, sourceFile); err != nil {
				log.Printf("[EOCleanJobs] eo_validation upsert %s: %v", it.emailLower, err)
			}
			if err := w.markItem(ctx, jobID, it.emailLower, "done", result); err != nil {
				log.Printf("[EOCleanJobs] item done %s: %v", it.emailLower, err)
			}
			done++
		case eoCleanActionFailed:
			if err := w.markItem(ctx, jobID, it.emailLower, "failed", result); err != nil {
				log.Printf("[EOCleanJobs] item failed %s: %v", it.emailLower, err)
			}
			failedN++
		default: // retry — stays pending
			if _, err := w.db.ExecContext(ctx, `
				UPDATE mailing_eo_clean_items SET attempts = attempts + 1
				WHERE job_id = $1::uuid AND email_lower = $2
			`, jobID, it.emailLower); err != nil {
				log.Printf("[EOCleanJobs] item retry %s: %v", it.emailLower, err)
			}
		}
	}
	if err := w.applyJobDelta(ctx, jobID, delta); err != nil {
		log.Printf("[EOCleanJobs] job counters %s: %v", jobID, err)
	}
	log.Printf("[EOCleanJobs] job=%s batch=%d done=%d failed=%d retry=%d (verified=%d complainer=%d undeliverable=%d other=%d)",
		jobID, len(items), done, failedN, len(items)-done-failedN,
		delta.verified, delta.complainer, delta.undeliverable, delta.other)
	return done, failedN
}

// upsertEOValidation lands one verdict in mailing_eo_validation with the
// EXACT newest-wins ON CONFLICT semantics of the jul21 import convention
// (agents/scheduling/eo_tranche_landing.py upsert_eo_validation).
func (w *EOCleanJobWorker) upsertEOValidation(ctx context.Context, emailLower, status, sourceFile string) error {
	_, err := w.db.ExecContext(ctx, `
		INSERT INTO mailing_eo_validation (email_lower, status, validated_at, source_file)
		VALUES ($1, $2, NOW(), $3)
		ON CONFLICT (email_lower) DO UPDATE SET
			status = EXCLUDED.status,
			validated_at = EXCLUDED.validated_at,
			source_file = EXCLUDED.source_file,
			imported_at = now()
		WHERE mailing_eo_validation.validated_at IS NULL
		   OR EXCLUDED.validated_at > mailing_eo_validation.validated_at
	`, emailLower, status, sourceFile)
	return err
}

// markItem stamps a terminal item state ('done' or 'failed') + the result.
// Every terminal stamp sets validated_at — the daily-budget recomputation
// counts these rows, so failed attempts spend budget too.
func (w *EOCleanJobWorker) markItem(ctx context.Context, jobID, emailLower, status, result string) error {
	_, err := w.db.ExecContext(ctx, `
		UPDATE mailing_eo_clean_items
		SET status = $3, result = $4, validated_at = NOW(), attempts = attempts + 1
		WHERE job_id = $1::uuid AND email_lower = $2
	`, jobID, emailLower, status, result)
	return err
}

// applyJobDelta folds one batch's counters into the job row. queued_count is
// the remaining-pending gauge: it drops by every item that left 'pending'.
func (w *EOCleanJobWorker) applyJobDelta(ctx context.Context, jobID string, d eoCleanDelta) error {
	if d.validated == 0 && d.failed == 0 {
		return nil
	}
	_, err := w.db.ExecContext(ctx, `
		UPDATE mailing_eo_clean_jobs SET
			validated_count     = validated_count + $2,
			verified_count      = verified_count + $3,
			complainer_count    = complainer_count + $4,
			undeliverable_count = undeliverable_count + $5,
			other_count         = other_count + $6,
			failed_count        = failed_count + $7,
			queued_count        = GREATEST(queued_count - $8, 0)
		WHERE id = $1::uuid
	`, jobID, d.validated, d.verified, d.complainer, d.undeliverable, d.other,
		d.failed, d.validated+d.failed)
	return err
}
