package worker

// ContentDeskWorker (2026-09-11) drives the Content Desk pipeline
// (internal/contentdesk): every tick it picks queued drafting articles
// (content_articles.run_requested_at) and runs research → rederive → draft →
// package → code_checks → judgment, ending at in_review. Nothing publishes —
// a human approves in the portal and a release is an admin action.
//
// Safety: CONTENT_DESK_ENABLED defaults OFF (no queue read, no lock, no model
// call); per-article distlock so two ECS tasks never run the same article;
// every stage is idempotent per (article, stage, input_hash) in
// content_pipeline_runs, so a double fire or a restart reuses finished stages
// instead of paying for them twice; bounded concurrency
// (CONTENT_DESK_CONCURRENCY, default 2); ctx cancellation stops the loop and
// is threaded into every model call.

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/contentdesk"
	"github.com/ignite/sparkpost-monitor/internal/pkg/distlock"
	"github.com/redis/go-redis/v9"
)

const (
	EnvContentDeskConcurrency  = "CONTENT_DESK_CONCURRENCY"
	contentDeskDefaultInterval = time.Minute
	// Longer than one pipeline pass is expected to take; a stage row that
	// outlives it is retaken only after contentdesk's StaleAfter (45m). 90m,
	// not 30m: with up to 5 revise rounds, trim passes and a repair round a
	// run takes 40m+ (live :1136: the history pilot ran 40m33s, the lock
	// expired mid-run, and a second task replayed the article and ran the
	// uncached agent review twice — two identical primary approvals).
	contentDeskLockTTL = 90 * time.Minute
)

// ContentDeskConcurrency is the bounded article concurrency (default 2, 1..8).
func ContentDeskConcurrency() int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(EnvContentDeskConcurrency))); err == nil && v > 0 {
		if v > 8 {
			return 8
		}
		return v
	}
	return 2
}

type contentDeskRunner interface {
	Run(ctx context.Context, org, articleID string) error
}

type contentDeskQueue interface {
	PendingArticles(ctx context.Context, limit int) ([]contentdesk.PendingArticle, error)
	StopRun(ctx context.Context, org, id string) error
}

type contentDeskPlanner interface {
	Plan(ctx context.Context, maxInFlight int) (int, error)
}

// contentDeskPlanEvery spaces planner passes (each files at most one brief per org).
const contentDeskPlanEvery = 10 * time.Minute

// ContentDeskWorker is the scheduler; the pipeline does the work.
type ContentDeskWorker struct {
	db          *sql.DB
	redis       *redis.Client
	queue       contentDeskQueue
	runner      contentDeskRunner
	planner     contentDeskPlanner // nil = no planner
	lastPlan    time.Time
	interval    time.Duration
	concurrency int
	enabled     func() bool
	newLock     func(key string) distlock.DistLock

	mu          sync.Mutex
	lastEnabled *bool

	// Article slots persist across ticks: a tick fills free slots and returns
	// without waiting (live :1145: the tick waited for its whole batch, so a
	// task running one 40-minute article left its other slot idle and three
	// re-queued articles sat unclaimed).
	slots    chan struct{}
	inflight map[string]bool // guarded by mu
	running  sync.WaitGroup
}

// NewContentDeskWorker wires the store, the Anthropic client (budget-gated by
// the same store) and the pipeline. redisClient may be nil — distlock falls
// back to a PG advisory lock.
func NewContentDeskWorker(db *sql.DB, redisClient *redis.Client) *ContentDeskWorker {
	store := contentdesk.NewStore(db)
	llm := contentdesk.NewAnthropicLLM(store)
	w := &ContentDeskWorker{
		db: db, redis: redisClient, queue: store,
		runner:      contentdesk.NewPipeline(store, llm),
		planner:     contentdesk.NewPlanner(store, llm),
		interval:    contentDeskDefaultInterval,
		concurrency: ContentDeskConcurrency(),
		enabled:     contentdesk.Enabled,
	}
	w.newLock = func(key string) distlock.DistLock { return distlock.NewLock(w.redis, w.db, key, contentDeskLockTTL) }
	return w
}

// Start runs the loop until ctx is cancelled. Non-blocking; no-op without db.
func (w *ContentDeskWorker) Start(ctx context.Context) {
	if w.db == nil {
		log.Printf("[ContentDesk] disabled (db missing)")
		return
	}
	go func() {
		log.Printf("Content Desk worker started (interval=%s, concurrency=%d, enabled=%v)", w.interval, w.concurrency, w.enabled())
		w.tick(ctx)
		t := time.NewTicker(w.interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				w.tick(ctx)
			case <-ctx.Done():
				log.Printf("[ContentDesk] context cancelled, stopping")
				return
			}
		}
	}()
}

func (w *ContentDeskWorker) noteEnabled(on bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.lastEnabled == nil || *w.lastEnabled != on {
		log.Printf("[ContentDesk] %s=%v — pipeline %s", contentdesk.EnvEnabled, on, map[bool]string{true: "running", false: "idle (no queue read, no model calls)"}[on])
		w.lastEnabled = &on
	}
}

// tick is one pass. Returns "cancelled" | "disabled" | "queue-error" | "idle" | "ran".
func (w *ContentDeskWorker) tick(ctx context.Context) string {
	if ctx.Err() != nil {
		return "cancelled"
	}
	on := w.enabled()
	w.noteEnabled(on)
	if !on {
		return "disabled"
	}
	w.maybePlan(ctx)
	arts, err := w.queue.PendingArticles(ctx, w.concurrency*4)
	if err != nil {
		log.Printf("[ContentDesk] ERROR step=queue: %v", err)
		return "queue-error"
	}
	if len(arts) == 0 {
		return "idle"
	}
	if w.slots == nil {
		w.slots = make(chan struct{}, w.concurrency)
	}
	launched := 0
	for _, a := range arts {
		if ctx.Err() != nil {
			break
		}
		w.mu.Lock()
		busy := w.inflight[a.ArticleID]
		w.mu.Unlock()
		if busy {
			continue
		}
		select {
		case w.slots <- struct{}{}:
		default:
			return map[bool]string{true: "ran", false: "busy"}[launched > 0] // every slot taken
		}
		w.mu.Lock()
		if w.inflight == nil {
			w.inflight = map[string]bool{}
		}
		w.inflight[a.ArticleID] = true
		w.mu.Unlock()
		launched++
		w.running.Add(1)
		go func(a contentdesk.PendingArticle) {
			defer w.running.Done()
			defer func() {
				w.mu.Lock()
				delete(w.inflight, a.ArticleID)
				w.mu.Unlock()
				<-w.slots
			}()
			w.runOne(ctx, a)
		}(a)
	}
	if launched == 0 {
		return "busy"
	}
	return "ran"
}

// maybePlan runs the brief planner at most every contentDeskPlanEvery, under a
// cluster-wide lock so two ECS tasks never file the same site's brief twice.
func (w *ContentDeskWorker) maybePlan(ctx context.Context) {
	if w.planner == nil || time.Since(w.lastPlan) < contentDeskPlanEvery {
		return
	}
	w.lastPlan = time.Now()
	lock := w.newLock("content_desk:planner")
	acquired, err := lock.Acquire(ctx)
	if err != nil || !acquired {
		return
	}
	defer func() { _ = lock.Release(context.Background()) }()
	if _, err := w.planner.Plan(ctx, w.concurrency); err != nil {
		log.Printf("[ContentDesk] ERROR step=planner: %v", err)
	}
}

// runOne leases one article and runs its pipeline. Returns the outcome label.
func (w *ContentDeskWorker) runOne(ctx context.Context, a contentdesk.PendingArticle) string {
	lock := w.newLock("content_desk:article:" + a.ArticleID)
	acquired, err := lock.Acquire(ctx)
	if err != nil {
		log.Printf("[ContentDesk] ERROR step=lock article=%s: %v", a.ArticleID, err)
		return "lock-error"
	}
	if !acquired {
		return "lock-held"
	}
	defer func() {
		if err := lock.Release(context.Background()); err != nil {
			log.Printf("[ContentDesk] ERROR step=lock-release article=%s: %v", a.ArticleID, err)
		}
	}()
	start := time.Now()
	err = w.runner.Run(ctx, a.OrgID, a.ArticleID)
	switch {
	case err == nil:
		log.Printf("[ContentDesk] article=%s → in_review (%s)", a.ArticleID, time.Since(start).Round(time.Second))
		return "done"
	case errors.Is(err, contentdesk.ErrInFlight):
		return "in-flight"
	case errors.Is(err, contentdesk.ErrRunExhausted), errors.Is(err, contentdesk.ErrRefused), errors.Is(err, contentdesk.ErrNoAllowlist):
		log.Printf("[ContentDesk] article=%s stopped (dequeued; re-run from the portal): %v", a.ArticleID, err)
		bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if serr := w.queue.StopRun(bg, a.OrgID, a.ArticleID); serr != nil {
			log.Printf("[ContentDesk] ERROR step=dequeue article=%s: %v", a.ArticleID, serr)
		}
		return "stopped"
	case errors.Is(err, contentdesk.ErrDisabled), errors.Is(err, contentdesk.ErrBudgetExceeded), errors.Is(err, contentdesk.ErrBudgetUnavailable):
		log.Printf("[ContentDesk] article=%s held (stays queued): %v", a.ArticleID, err)
		return "held"
	default:
		log.Printf("[ContentDesk] ERROR article=%s (retries next tick until the stage exhausts): %v", a.ArticleID, err)
		return "failed"
	}
}
