package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"sync"
	"time"
)

// CampaignHealthMonitor runs in the background alongside the wave scheduler
// and auto-pauses campaigns with dangerously high bounce rates. It also
// records execution metrics back to EDITH recommendations once campaigns
// finish, closing the learning feedback loop.
type CampaignHealthMonitor struct {
	db       *sql.DB
	interval time.Duration
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

func NewCampaignHealthMonitor(db *sql.DB) *CampaignHealthMonitor {
	return &CampaignHealthMonitor{
		db:       db,
		interval: 60 * time.Second,
	}
}

func (m *CampaignHealthMonitor) Start() {
	m.ctx, m.cancel = context.WithCancel(context.Background())
	m.wg.Add(1)
	go m.loop()
	log.Println("[HealthMonitor] started")
}

func (m *CampaignHealthMonitor) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
	m.wg.Wait()
	log.Println("[HealthMonitor] stopped")
}

func (m *CampaignHealthMonitor) loop() {
	defer m.wg.Done()
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.checkCampaigns()
			m.recordCompletedMetrics()
		}
	}
}

const (
	autoPauseBounceRate    = 0.10  // 10%
	warningBounceRate      = 0.05  // 5%
	autoPauseMinSent       = 100
	autoPauseWindowMinutes = 30
)

// checkCampaigns evaluates all active PMTA wave campaigns and auto-pauses
// any that exceed the bounce rate threshold in their first 30 minutes.
func (m *CampaignHealthMonitor) checkCampaigns() {
	ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
	defer cancel()

	rows, err := m.db.QueryContext(ctx, `
		SELECT id, sent_count,
		       COALESCE(bounce_count, 0),
		       COALESCE(hard_bounce_count, 0),
		       COALESCE(started_at, created_at)
		FROM mailing_campaigns
		WHERE status = 'sending'
		  AND execution_mode = 'pmta_isp_wave'
	`)
	if err != nil {
		log.Printf("[HealthMonitor] query error: %v", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var id string
		var sentCount, bounceCount, hardBounceCount int
		var startedAt time.Time
		if err := rows.Scan(&id, &sentCount, &bounceCount, &hardBounceCount, &startedAt); err != nil {
			continue
		}

		if sentCount < autoPauseMinSent {
			continue
		}

		bounceRate := float64(bounceCount) / float64(sentCount)
		minutesSinceStart := time.Since(startedAt).Minutes()

		if bounceRate > autoPauseBounceRate && minutesSinceStart <= autoPauseWindowMinutes {
			log.Printf("[HealthMonitor] AUTO-PAUSING campaign %s: bounce_rate=%.2f%% sent=%d bounced=%d (%.0f min since start)",
				id, bounceRate*100, sentCount, bounceCount, minutesSinceStart)
			m.pauseCampaign(ctx, id)
			continue
		}

		if bounceRate > warningBounceRate {
			log.Printf("[HealthMonitor] WARNING campaign %s: bounce_rate=%.2f%% sent=%d bounced=%d",
				id, bounceRate*100, sentCount, bounceCount)
			m.db.ExecContext(ctx, `
				UPDATE mailing_campaigns
				SET pmta_config = COALESCE(pmta_config, '{}'::jsonb) ||
				    jsonb_build_object('health_warning',
				        jsonb_build_object('bounce_rate', $2::text, 'checked_at', NOW()::text)),
				    updated_at = NOW()
				WHERE id = $1
			`, id, bounceRate)
		}
	}
}

// pauseCampaign mirrors the CampaignBuilder.HandlePauseCampaign logic:
// sets campaign status to 'paused', pauses queued items and running ISP plans.
func (m *CampaignHealthMonitor) pauseCampaign(ctx context.Context, campaignID string) {
	_, err := m.db.ExecContext(ctx, `
		UPDATE mailing_campaigns SET status = 'paused', updated_at = NOW()
		WHERE id = $1 AND status = 'sending'
	`, campaignID)
	if err != nil {
		log.Printf("[HealthMonitor] pause campaign %s error: %v", campaignID, err)
		return
	}
	m.db.ExecContext(ctx, `
		UPDATE mailing_campaign_queue SET status = 'paused', updated_at = NOW()
		WHERE campaign_id = $1 AND status = 'queued'
	`, campaignID)
	m.db.ExecContext(ctx, `
		UPDATE mailing_campaign_isp_plans SET status = 'paused', updated_at = NOW()
		WHERE campaign_id = $1 AND status = 'running'
	`, campaignID)
	m.db.ExecContext(ctx, `
		UPDATE mailing_campaign_waves SET status = 'cancelled', updated_at = NOW()
		WHERE campaign_id = $1 AND status = 'planned'
	`, campaignID)
}

// recordCompletedMetrics finds EDITH recommendations whose linked campaigns
// have finished and writes final execution metrics back, so EDITH can learn
// from past outcomes.
func (m *CampaignHealthMonitor) recordCompletedMetrics() {
	ctx, cancel := context.WithTimeout(m.ctx, 15*time.Second)
	defer cancel()

	rows, err := m.db.QueryContext(ctx, `
		SELECT r.id, r.executed_campaign_id, c.status,
		       c.sent_count,
		       COALESCE(c.delivered_count, 0),
		       COALESCE(c.bounce_count, 0),
		       COALESCE(c.hard_bounce_count, 0),
		       COALESCE(c.soft_bounce_count, 0),
		       COALESCE(c.open_count, 0),
		       COALESCE(c.click_count, 0),
		       COALESCE(c.complaint_count, 0)
		FROM agent_campaign_recommendations r
		JOIN mailing_campaigns c ON c.id = r.executed_campaign_id
		WHERE r.status = 'approved'
		  AND c.status IN ('completed', 'sent', 'paused', 'failed', 'cancelled')
		  AND r.execution_metrics IS NULL
	`)
	if err != nil {
		return
	}
	defer rows.Close()

	for rows.Next() {
		var recID, campID, campStatus string
		var sent, delivered, bounced, hardBounces, softBounces, opens, clicks, complaints int
		if err := rows.Scan(&recID, &campID, &campStatus, &sent, &delivered, &bounced, &hardBounces, &softBounces, &opens, &clicks, &complaints); err != nil {
			continue
		}

		metrics := map[string]interface{}{
			"sent":          sent,
			"delivered":     delivered,
			"hard_bounces":  hardBounces,
			"soft_bounces":  softBounces,
			"opens":         opens,
			"clicks":        clicks,
			"complaints":    complaints,
			"recorded_at":   time.Now().Format(time.RFC3339),
		}
		metricsJSON, _ := json.Marshal(metrics)

		newStatus := "completed"
		if campStatus == "failed" || campStatus == "cancelled" {
			newStatus = "failed"
		}

		m.db.ExecContext(ctx, `
			UPDATE agent_campaign_recommendations
			SET execution_metrics = $2::jsonb, status = $3, updated_at = NOW()
			WHERE id = $1
		`, recID, string(metricsJSON), newStatus)
	}
}
