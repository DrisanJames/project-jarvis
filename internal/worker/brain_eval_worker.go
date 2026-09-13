package worker

// BrainEvalWorker (operator 2026-09-12, the brain's retained-competence check).
//
// Once a day, under a distlock lease, re-run every active jarvis_brain_evals
// row through brain.Runner and record the run. A failing eval means either the
// platform regressed or the pinned expectation aged out — both are findings
// the operator reads on the Brain tab / via the console. The worker never
// edits an eval; re-baselining is a human decision recorded as a new as_of.
//
// Kill switch: BRAIN_EVAL_DISABLED. Heartbeat: mailing_worker_heartbeats
// 'brain_eval'.

import (
	"context"
	"database/sql"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/brain"
	"github.com/ignite/sparkpost-monitor/internal/pkg/distlock"
	"github.com/redis/go-redis/v9"
)

const (
	brainEvalWorkerName          = "brain_eval"
	brainEvalLockKey             = "brain_eval"
	DefaultBrainEvalInterval     = 24 * time.Hour
	brainEvalPerRunLeaseMinimum  = 30 * time.Minute
	brainEvalMaxEvalsPerPass     = 500
	brainEvalHeartbeatIntervalHz = 3 // monitor flags did-not-run at 3x cadence (worker_health.go)
)

func brainEvalDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("BRAIN_EVAL_DISABLED"))) {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}

type BrainEvalWorker struct {
	db       *sql.DB
	redis    *redis.Client
	interval time.Duration
	runner   *brain.Runner
	store    *brain.Store
}

func NewBrainEvalWorker(db *sql.DB, redisClient *redis.Client, runner *brain.Runner) *BrainEvalWorker {
	return &BrainEvalWorker{db: db, redis: redisClient, interval: DefaultBrainEvalInterval,
		runner: runner, store: brain.NewStore(db)}
}

func (w *BrainEvalWorker) WithInterval(d time.Duration) *BrainEvalWorker {
	if d > 0 {
		w.interval = d
	}
	return w
}

func (w *BrainEvalWorker) Start(ctx context.Context) {
	if w.db == nil || w.runner == nil {
		log.Printf("[BrainEval] disabled (db or runner missing)")
		return
	}
	go func() {
		log.Printf("Brain eval worker started (interval=%s)", w.interval)
		w.tick(ctx)
		t := time.NewTicker(w.interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				w.tick(ctx)
			}
		}
	}()
}

func (w *BrainEvalWorker) tick(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	if brainEvalDisabled() {
		EmitHeartbeat(ctx, w.db, brainEvalWorkerName, int(w.interval.Seconds()), "disabled", "BRAIN_EVAL_DISABLED set")
		return
	}
	lease := w.interval
	if lease < brainEvalPerRunLeaseMinimum {
		lease = brainEvalPerRunLeaseMinimum
	}
	lock := distlock.NewLock(w.redis, w.db, brainEvalLockKey, lease)
	acquired, err := lock.Acquire(ctx)
	if err != nil {
		log.Printf("[BrainEval] lock acquire error: %v", err)
		return
	}
	if !acquired {
		return
	}
	defer func() {
		if err := lock.Release(context.Background()); err != nil {
			log.Printf("[BrainEval] lock release error: %v", err)
		}
	}()
	w.RunOnce(ctx)
}

// RunOnce executes every active eval once. Exported for tests and the API's
// "run all now"; the caller owns locking.
func (w *BrainEvalWorker) RunOnce(ctx context.Context) (passed, failed int) {
	evals, err := w.store.ListEvalsAllOrgs(ctx)
	if err != nil {
		log.Printf("[BrainEval] list evals: %v", err)
		EmitHeartbeat(ctx, w.db, brainEvalWorkerName, int(w.interval.Seconds()), "error", "list: "+err.Error())
		return 0, 0
	}
	if len(evals) > brainEvalMaxEvalsPerPass {
		evals = evals[:brainEvalMaxEvalsPerPass]
	}
	for _, e := range evals {
		if ctx.Err() != nil {
			break
		}
		run := w.runner.Run(ctx, e)
		if run.Pass {
			passed++
		} else {
			failed++
			log.Printf("[BrainEval] FAIL %s (org %s): %s", e.Name, e.OrgID, run.Error)
		}
	}
	status := "ok"
	msg := ""
	if failed > 0 {
		status = "error"
		msg = strconv.Itoa(failed) + " eval(s) failing"
	}
	EmitHeartbeat(ctx, w.db, brainEvalWorkerName, int(w.interval.Seconds()), status, msg)
	log.Printf("[BrainEval] pass complete: %d passed, %d failed", passed, failed)
	return passed, failed
}
