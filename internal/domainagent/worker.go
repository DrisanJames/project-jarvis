package domainagent

import (
	"context"
	"database/sql"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	scorecardLockKey  = "lock:domain-agent-scorecard"
	scorecardLockTTL  = 10 * time.Minute
	scorecardPGLockID = 987654331
	scorecardInterval = 6 * time.Hour
	scorecardWarmup   = 60 * time.Second
	// rollupWindowDays: each run rebuilds today plus the two prior days so
	// late-arriving tracking events (bounce webhooks, delayed opens) are
	// folded into already-rolled-up days.
	rollupWindowDays = 3
)

// ScorecardWorker keeps mailing_domain_agent_scorecard fresh: first run 60s
// after boot, then every 6h, rebuilding the trailing 3-day window for every
// org with at least one active sending profile. Redis SetNX distlock when
// available; pg_try_advisory_lock fallback otherwise.
type ScorecardWorker struct {
	db          *sql.DB
	redisClient *redis.Client
}

func NewScorecardWorker(db *sql.DB, redisClient *redis.Client) *ScorecardWorker {
	return &ScorecardWorker{db: db, redisClient: redisClient}
}

// Start launches the rollup loop in its own goroutine.
func (w *ScorecardWorker) Start(ctx context.Context) {
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(scorecardWarmup):
		}
		w.runOnce(ctx)
		ticker := time.NewTicker(scorecardInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				w.runOnce(ctx)
			}
		}
	}()
}

func (w *ScorecardWorker) runOnce(ctx context.Context) {
	acquired, release := w.acquireLock(ctx)
	if !acquired {
		log.Println("[DomainAgent] scorecard rollup skipped — another instance holds the lock")
		return
	}
	defer release()

	rows, err := w.db.QueryContext(ctx, `
		SELECT DISTINCT organization_id
		FROM mailing_sending_profiles
		WHERE status = 'active'
	`)
	if err != nil {
		log.Printf("[DomainAgent] org enumeration failed: %v", err)
		return
	}
	var orgs []string
	for rows.Next() {
		var org string
		if err := rows.Scan(&org); err != nil {
			log.Printf("[DomainAgent] org scan failed: %v", err)
			continue
		}
		orgs = append(orgs, org)
	}
	rows.Close()

	today := utcDate(time.Now())
	from := today.AddDate(0, 0, -(rollupWindowDays - 1))
	for _, org := range orgs {
		if err := RollupRange(ctx, w.db, org, from, today); err != nil {
			log.Printf("[DomainAgent] rollup for org %s completed with errors: %v", org, err)
		}
	}
	log.Printf("[DomainAgent] scorecard cycle complete: %d org(s), window %s → %s",
		len(orgs), from.Format("2006-01-02"), today.Format("2006-01-02"))
}

// acquireLock returns whether the cross-instance lock was won and a release
// func. Redis SetNX with TTL preferred; PG advisory lock fallback.
func (w *ScorecardWorker) acquireLock(ctx context.Context) (bool, func()) {
	noop := func() {}
	if w.redisClient != nil {
		ok, err := w.redisClient.SetNX(ctx, scorecardLockKey, "1", scorecardLockTTL).Result()
		if err == nil {
			if !ok {
				return false, noop
			}
			return true, func() { w.redisClient.Del(context.Background(), scorecardLockKey) }
		}
		log.Printf("[DomainAgent] redis lock failed (%v) — falling back to pg advisory lock", err)
	}
	var acquired bool
	if err := w.db.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", scorecardPGLockID).Scan(&acquired); err != nil {
		log.Printf("[DomainAgent] advisory lock query failed: %v — skipping cycle", err)
		return false, noop
	}
	if !acquired {
		return false, noop
	}
	return true, func() {
		w.db.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", scorecardPGLockID)
	}
}
