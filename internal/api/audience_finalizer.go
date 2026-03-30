package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"time"

)

// StartAudienceWorker polls for campaigns in 'finalizing_audience' status and
// processes them: builds the audience snapshot, creates waves, transitions to
// 'scheduled'. This replaces the fire-and-forget finalizeDeploy goroutine.
func (s *PMTACampaignService) StartAudienceWorker(ctx context.Context) {
	log.Println("[AudienceWorker] started (poll=15s)")
	go func() {
		time.Sleep(10 * time.Second)
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.processNextFinalizingCampaign(ctx)
			case <-ctx.Done():
				log.Println("[AudienceWorker] context cancelled, stopping")
				return
			}
		}
	}()
}

func (s *PMTACampaignService) processNextFinalizingCampaign(parentCtx context.Context) {
	// Atomically claim one campaign: find + update status in one query.
	// This prevents two ECS containers from processing the same campaign.
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

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

	// Use a dedicated connection with extended timeouts. The pool-level DSN
	// sets statement_timeout=30s which is too short for audience planning.
	conn, connErr := s.db.Conn(ctx)
	if connErr != nil {
		log.Printf("[AudienceWorker] get connection failed for %s: %v", campaignID, connErr)
		s.markCampaignFailed(campaignID, "database connection error")
		return
	}
	defer func() {
		conn.ExecContext(context.Background(), "RESET ALL")
		conn.Close()
	}()
	if _, err := conn.ExecContext(ctx, "SET statement_timeout = '1200000'"); err != nil {
		log.Printf("[AudienceWorker] SET statement_timeout failed: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "SET idle_in_transaction_session_timeout = '1200000'"); err != nil {
		log.Printf("[AudienceWorker] SET idle_in_transaction_session_timeout failed: %v", err)
	}

	var audienceDB dbQuerier = conn

	audience, err := planPMTAAudience(ctx, audienceDB, orgID, input, normalized, s.suppMatcher, s.offerSuppMgr)
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

	log.Printf("[AudienceWorker] campaign %s audience ready: %d recipients across %d ISPs",
		campaignID, audience.SelectedTotal, len(audience.CountsByISP))

	// Campaign is already in 'preparing' (set during claim). Reset to 'draft'
	// so resolvePMTACampaignIdentity accepts it for wave creation.
	if _, err := s.db.ExecContext(ctx, `UPDATE mailing_campaigns SET status = 'draft' WHERE id = $1 AND status = 'preparing'`, campaignID); err != nil {
		log.Printf("[AudienceWorker] failed to reset status for %s: %v", campaignID, err)
		s.markCampaignFailed(campaignID, "internal error preparing campaign")
		return
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		log.Printf("[AudienceWorker] begin tx failed for %s: %v", campaignID, err)
		s.markCampaignFailed(campaignID, "internal error: "+err.Error())
		return
	}
	defer tx.Rollback()

	result, err := createPMTAWaveCampaign(ctx, tx, s.db, orgID, input, normalized, audience, s.colCache)
	if err != nil {
		log.Printf("[AudienceWorker] create wave campaign failed for %s: %v", campaignID, err)
		s.markCampaignFailed(campaignID, "campaign creation failed: "+err.Error())
		return
	}

	if err := tx.Commit(); err != nil {
		log.Printf("[AudienceWorker] commit failed for %s: %v", campaignID, err)
		s.markCampaignFailed(campaignID, "commit failed: "+err.Error())
		return
	}

	log.Printf("[AudienceWorker] campaign %s finalized: audience=%d status=%s",
		campaignID, result.TotalAudience, result.Status)
}

