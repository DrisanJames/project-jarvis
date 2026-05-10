package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"os"
	"strconv"
	"time"
)

// StartAudienceWorker spawns a pool of goroutines that each poll for campaigns
// in 'finalizing_audience' status and process them: build the audience snapshot,
// create waves, transition to 'scheduled'. The pool size is controlled by
// AUDIENCE_WORKER_COUNT (default 4, clamped to [1,16]). Concurrent claims are
// safe because processNextFinalizingCampaign uses FOR UPDATE SKIP LOCKED.
func (s *PMTACampaignService) StartAudienceWorker(ctx context.Context) {
	count := audienceWorkerCount()
	log.Printf("[AudienceWorker] started (workers=%d, poll=15s)", count)

	// Connection pool sanity check: each worker may hold up to 3 conns at peak
	// (poll/claim, planner, wave-create tx). Warn if the pool is too small.
	if stats := s.db.Stats(); stats.MaxOpenConnections > 0 && stats.MaxOpenConnections < count*3 {
		log.Printf("[AudienceWorker] WARNING: max_open_connections=%d may be insufficient for %d workers (recommend >= %d)",
			stats.MaxOpenConnections, count, count*3)
	}

	for i := 0; i < count; i++ {
		go s.audienceWorkerLoop(ctx, i)
	}
}

// audienceWorkerCount reads AUDIENCE_WORKER_COUNT and returns the configured
// worker pool size. Returns 4 if the env is unset, unparseable, or out of the
// supported range [1,16].
func audienceWorkerCount() int {
	if v := os.Getenv("AUDIENCE_WORKER_COUNT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 16 {
			return n
		}
	}
	return 4
}

// audienceWorkerLoop runs a single finalizer goroutine. The initial delay
// staggers workers across the 15-second polling window so they don't all hit
// the FOR UPDATE SKIP LOCKED query simultaneously. Capped at 12s of stagger
// regardless of idx so the pool is fully warm within ~25 seconds of startup.
func (s *PMTACampaignService) audienceWorkerLoop(ctx context.Context, idx int) {
	stagger := time.Duration(idx) * 3 * time.Second
	if stagger > 12*time.Second {
		stagger = 12 * time.Second
	}
	initialDelay := 10*time.Second + stagger

	// Context-aware sleep so cancellation drains the pool quickly during
	// shutdown rather than waiting out the full initial delay.
	select {
	case <-time.After(initialDelay):
	case <-ctx.Done():
		log.Printf("[AudienceWorker-%d] context cancelled during initial delay, stopping", idx)
		return
	}

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.processNextFinalizingCampaign(ctx)
		case <-ctx.Done():
			log.Printf("[AudienceWorker-%d] context cancelled, stopping", idx)
			return
		}
	}
}

func (s *PMTACampaignService) processNextFinalizingCampaign(parentCtx context.Context) {
	// Recover campaigns orphaned in 'preparing' by dead goroutines. The 45-min
	// threshold exceeds the 30-min processing context + 10-sec markFailed window,
	// guaranteeing no live goroutine is still working on the campaign.
	if res, err := s.db.ExecContext(parentCtx, `
		UPDATE mailing_campaigns
		SET status = 'finalizing_audience', updated_at = NOW()
		WHERE status = 'preparing'
		  AND updated_at < NOW() - INTERVAL '45 minutes'
	`); err == nil {
		if n, _ := res.RowsAffected(); n > 0 {
			log.Printf("[AudienceWorker] recovered %d stale preparing campaign(s)", n)
		}
	}

	var campaignID, orgID string
	var configJSON sql.NullString

	err := s.db.QueryRowContext(parentCtx, `
		UPDATE mailing_campaigns
		SET status = 'preparing', updated_at = NOW()
		WHERE id = (
			SELECT id FROM mailing_campaigns
			WHERE status = 'finalizing_audience'
			ORDER BY created_at ASC
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id::text, organization_id::text, pmta_config::text
	`).Scan(&campaignID, &orgID, &configJSON)

	if err == sql.ErrNoRows {
		return
	}
	if err != nil {
		log.Printf("[AudienceWorker] poll error: %v", err)
		return
	}

	if !configJSON.Valid || configJSON.String == "" {
		log.Printf("[AudienceWorker] campaign %s has no pmta_config, marking failed", campaignID)
		s.markCampaignFailed(campaignID, "no campaign configuration found")
		return
	}

	log.Printf("[AudienceWorker] picked up campaign %s", campaignID)
	s.finalizeAudience(campaignID, orgID, configJSON.String)
}

func (s *PMTACampaignService) finalizeAudience(campaignID, orgID, configRaw string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[AudienceWorker] PANIC finalizing campaign %s: %v", campaignID, r)
			s.markCampaignFailed(campaignID, "internal panic during audience finalization")
		}
	}()

	var cfg pmtaCampaignConfig
	if err := json.Unmarshal([]byte(configRaw), &cfg); err != nil {
		log.Printf("[AudienceWorker] unmarshal config for %s: %v", campaignID, err)
		s.markCampaignFailed(campaignID, "invalid campaign configuration: "+err.Error())
		return
	}
	input := cfg.CampaignInput
	input.CampaignID = campaignID

	normalized, err := normalizePMTACampaignInput(input)
	if err != nil {
		log.Printf("[AudienceWorker] normalize failed for %s: %v", campaignID, err)
		s.markCampaignFailed(campaignID, "normalization failed: "+err.Error())
		return
	}

	// ── Phase 1: Audience Planning (own 20-min context) ──
	planStart := time.Now()
	planCtx, planCancel := context.WithTimeout(context.Background(), 20*time.Minute)

	conn, connErr := s.db.Conn(planCtx)
	if connErr != nil {
		planCancel()
		log.Printf("[AudienceWorker] get connection failed for %s: %v", campaignID, connErr)
		s.markCampaignFailed(campaignID, "database connection error")
		return
	}
	if _, err := conn.ExecContext(planCtx, "SET statement_timeout = '1200000'"); err != nil {
		log.Printf("[AudienceWorker] SET statement_timeout failed: %v", err)
	}
	if _, err := conn.ExecContext(planCtx, "SET idle_in_transaction_session_timeout = '1200000'"); err != nil {
		log.Printf("[AudienceWorker] SET idle_in_transaction_session_timeout failed: %v", err)
	}

	audience, err := planPMTAAudience(planCtx, conn, orgID, input, normalized, s.suppMatcher, s.globalHub, s.offerSuppMgr)

	// Close Phase 1 conn immediately to free the pool slot before Phase 2.
	resetCtx, resetCancel := context.WithTimeout(context.Background(), 5*time.Second)
	conn.ExecContext(resetCtx, "RESET ALL")
	conn.Close()
	resetCancel()
	planCancel()

	if err != nil {
		log.Printf("[AudienceWorker] audience planning failed for %s: %v", campaignID, err)
		s.markCampaignFailed(campaignID, "audience planning failed: "+err.Error())
		return
	}

	if audience.SelectedTotal == 0 {
		log.Printf("[AudienceWorker] campaign %s has 0 qualified recipients", campaignID)
		s.markCampaignFailed(campaignID, "no qualified recipients found")
		return
	}

	log.Printf("[AudienceWorker] campaign %s Phase 1 complete: audience=%d/%d in %v",
		campaignID, audience.SelectedTotal, audience.TotalSeen, time.Since(planStart))

	// ── Phase 2: Wave Creation (own 20-min context) ──
	waveStart := time.Now()
	waveCtx, waveCancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer waveCancel()

	tx, err := s.db.BeginTx(waveCtx, nil)
	if err != nil {
		log.Printf("[AudienceWorker] begin tx failed for %s: %v", campaignID, err)
		s.markCampaignFailed(campaignID, "internal error: "+err.Error())
		return
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(waveCtx, "SET LOCAL statement_timeout = '1200000'"); err != nil {
		log.Printf("[AudienceWorker] SET LOCAL statement_timeout failed for %s: %v — aborting (tx would use 30s default)", campaignID, err)
		tx.Rollback()
		s.markCampaignFailed(campaignID, "internal error: could not set transaction timeout")
		return
	}
	if _, err := tx.ExecContext(waveCtx, "SET LOCAL idle_in_transaction_session_timeout = '1200000'"); err != nil {
		log.Printf("[AudienceWorker] SET LOCAL idle_in_transaction failed for %s: %v", campaignID, err)
	}

	result, err := createPMTAWaveCampaign(waveCtx, tx, s.db, orgID, input, normalized, audience, s.colCache)
	if err != nil {
		log.Printf("[AudienceWorker] create wave campaign failed for %s: %v", campaignID, err)
		tx.Rollback()
		s.markCampaignFailed(campaignID, "campaign creation failed: "+err.Error())
		return
	}

	if err := tx.Commit(); err != nil {
		log.Printf("[AudienceWorker] commit failed for %s: %v", campaignID, err)
		s.markCampaignFailed(campaignID, "commit failed: "+err.Error())
		return
	}

	log.Printf("[AudienceWorker] campaign %s Phase 2 complete: wave creation in %v",
		campaignID, time.Since(waveStart))
	log.Printf("[AudienceWorker] campaign %s finalized: audience=%d status=%s",
		campaignID, result.TotalAudience, result.Status)
}

