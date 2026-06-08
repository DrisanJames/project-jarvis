package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

type CampaignHealthMonitor struct {
	db       *sql.DB
	interval time.Duration
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup

	// Lateness-alerting configuration. Zero value = feature disabled.
	latenessEnabled    bool
	latenessThreshold  time.Duration // e.g. 5 * time.Minute
	latenessReAlert    time.Duration // e.g. 6 * time.Hour
	latenessAlerter    SMSAlerter    // nil = feature disabled
	latenessRecipients []string      // SMS recipients (E.164)
}

// SMSAlerter is the minimum surface the health monitor needs in order to
// page someone when a campaign doesn't fire on time. Implemented by
// internal/pkg/twilio.Client; also trivially mockable in tests.
type SMSAlerter interface {
	SendSMS(ctx context.Context, to, body string) (string, error)
}

func NewCampaignHealthMonitor(db *sql.DB) *CampaignHealthMonitor {
	return &CampaignHealthMonitor{
		db:       db,
		interval: 60 * time.Second,
	}
}

// SetLatenessAlerter enables the campaign-lateness SMS alert. Safe to call
// after Start; the next tick will pick it up. If alerter is nil or recipients
// is empty, lateness alerting stays disabled.
func (m *CampaignHealthMonitor) SetLatenessAlerter(
	alerter SMSAlerter,
	recipients []string,
	threshold time.Duration,
	reAlert time.Duration,
) {
	if alerter == nil || len(recipients) == 0 {
		m.latenessEnabled = false
		return
	}
	if threshold <= 0 {
		threshold = 5 * time.Minute
	}
	if reAlert <= 0 {
		reAlert = 6 * time.Hour
	}
	m.latenessAlerter = alerter
	m.latenessRecipients = append([]string(nil), recipients...)
	m.latenessThreshold = threshold
	m.latenessReAlert = reAlert
	m.latenessEnabled = true
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
			m.checkISPThresholds()
			m.recordCompletedMetrics()
			m.recordListQualityMetrics()
			m.checkLateCampaigns()
		}
	}
}

const (
	autoPauseBounceRate    = 0.10
	warningBounceRate      = 0.05
	autoPauseMinSent       = 100
	autoPauseWindowMinutes = 30
)

func (m *CampaignHealthMonitor) checkCampaigns() {
	ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
	defer cancel()

	rows, err := m.db.QueryContext(ctx, `
		SELECT id, sent_count,
		       COALESCE(bounce_count, 0),
		       CASE WHEN COALESCE(hard_bounce_count,0)+COALESCE(soft_bounce_count,0)>0 THEN COALESCE(hard_bounce_count,0) ELSE COALESCE(bounce_count,0) END,
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
			log.Printf("[HealthMonitor] HIGH BOUNCE campaign %s: bounce_rate=%.2f%% sent=%d bounced=%d (%.0f min since start) — auto-pause DISABLED, manual intervention required",
				id, bounceRate*100, sentCount, bounceCount, minutesSinceStart)
		}

		if bounceRate > warningBounceRate {
			var trueHard, repBlocked, soft int
			_ = m.db.QueryRowContext(ctx, `
				SELECT COALESCE(SUM(hard_bounced),0),
				       COALESCE(SUM(reputation_blocked),0),
				       COALESCE(SUM(soft_bounced),0)
				FROM pmta_acct_daily_summary
				WHERE campaign_id = $1::uuid
			`, id).Scan(&trueHard, &repBlocked, &soft)

			log.Printf("[HealthMonitor] WARNING campaign %s: bounce_rate=%.2f%% sent=%d bounced=%d (hard=%d reputation_blocked=%d soft=%d)",
				id, bounceRate*100, sentCount, bounceCount, trueHard, repBlocked, soft)
			m.db.ExecContext(ctx, `
				UPDATE mailing_campaigns
				SET pmta_config = COALESCE(pmta_config, '{}'::jsonb) ||
				    jsonb_build_object('health_warning',
				        jsonb_build_object('bounce_rate', $2::text, 'true_hard', $3::text,
				            'reputation_blocked', $4::text, 'checked_at', NOW()::text)),
				    updated_at = NOW()
				WHERE id = $1
			`, id, bounceRate, trueHard, repBlocked)
		}
	}
}

type deliveryThresholdsCfg struct {
	DeferralPausePct   float64 `json:"deferral_pause_pct"`
	BlockPausePct      float64 `json:"block_pause_pct"`
	ComplaintPausePct  float64 `json:"complaint_pause_pct"`
	CheckWindowMinutes int     `json:"check_window_minutes"`
	MinSentForCheck    int     `json:"min_sent_for_check"`
}

type ispEventCounts struct {
	ISP        string
	Delivered  int
	Deferred   int
	Bounced    int
	Complained int
}

func (m *CampaignHealthMonitor) checkISPThresholds() {
	ctx, cancel := context.WithTimeout(m.ctx, 45*time.Second)
	defer cancel()

	rows, err := m.db.QueryContext(ctx, `
		SELECT c.id, c.pmta_config, COALESCE(c.started_at, c.created_at)
		FROM mailing_campaigns c
		WHERE c.status = 'sending'
		  AND c.execution_mode = 'pmta_isp_wave'
		  AND c.pmta_config IS NOT NULL
		  AND c.pmta_config->'campaign_input'->'delivery_thresholds' IS NOT NULL
	`)
	if err != nil {
		if !strings.Contains(err.Error(), "does not exist") {
			log.Printf("[HealthMonitor] isp-threshold query error: %v", err)
		}
		return
	}
	defer rows.Close()

	for rows.Next() {
		var campID string
		var pmtaConfigRaw []byte
		var startedAt time.Time
		if err := rows.Scan(&campID, &pmtaConfigRaw, &startedAt); err != nil {
			continue
		}

		var cfg struct {
			CampaignInput struct {
				DeliveryThresholds *deliveryThresholdsCfg `json:"delivery_thresholds"`
			} `json:"campaign_input"`
		}
		if err := json.Unmarshal(pmtaConfigRaw, &cfg); err != nil || cfg.CampaignInput.DeliveryThresholds == nil {
			continue
		}

		thresh := cfg.CampaignInput.DeliveryThresholds
		windowMin := thresh.CheckWindowMinutes
		if windowMin <= 0 {
			windowMin = 30
		}
		minSent := thresh.MinSentForCheck
		if minSent <= 0 {
			minSent = 100
		}

		if time.Since(startedAt).Minutes() > float64(windowMin) {
			continue
		}

		counts, err := m.getISPEventCounts(ctx, campID)
		if err != nil {
			log.Printf("[HealthMonitor] failed to get ISP events for %s: %v", campID, err)
			continue
		}

		for _, c := range counts {
			total := c.Delivered + c.Deferred + c.Bounced
			if total < minSent {
				continue
			}

			// POLICY: automated queue pausing is disabled platform-wide. Threshold
			// breaches are logged for operator review only; no campaign/ISP is paused.
			if thresh.DeferralPausePct > 0 {
				rate := float64(c.Deferred) / float64(total) * 100
				if rate > thresh.DeferralPausePct {
					log.Printf("[HealthMonitor] ISP %s on campaign %s: deferral_rate=%.1f%% exceeded threshold=%.1f%% (delivered=%d deferred=%d) — auto-pause DISABLED, manual intervention required",
						c.ISP, campID, rate, thresh.DeferralPausePct, c.Delivered, c.Deferred)
					continue
				}
			}

			if thresh.BlockPausePct > 0 {
				rate := float64(c.Bounced) / float64(total) * 100
				if rate > thresh.BlockPausePct {
					log.Printf("[HealthMonitor] ISP %s on campaign %s: block_rate=%.1f%% exceeded threshold=%.1f%% (total=%d bounced=%d) — auto-pause DISABLED, manual intervention required",
						c.ISP, campID, rate, thresh.BlockPausePct, total, c.Bounced)
					continue
				}
			}

			if thresh.ComplaintPausePct > 0 && c.Delivered > 0 {
				rate := float64(c.Complained) / float64(c.Delivered) * 100
				if rate > thresh.ComplaintPausePct {
					log.Printf("[HealthMonitor] ISP %s on campaign %s: complaint_rate=%.2f%% exceeded threshold=%.2f%% (delivered=%d complaints=%d) — auto-pause DISABLED, manual intervention required",
						c.ISP, campID, rate, thresh.ComplaintPausePct, c.Delivered, c.Complained)
					continue
				}
			}
		}
	}
}

func (m *CampaignHealthMonitor) getISPEventCounts(ctx context.Context, campaignID string) ([]ispEventCounts, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT
			COALESCE(p.recipient_isp, 'unknown') AS isp,
			COUNT(*) FILTER (WHERE te.event_type = 'delivered') AS delivered,
			COUNT(*) FILTER (WHERE te.event_type = 'deferred')  AS deferred,
			COUNT(*) FILTER (WHERE te.event_type = 'bounced')   AS bounced,
			COUNT(*) FILTER (WHERE te.event_type = 'complained') AS complained
		FROM mailing_tracking_events te
		JOIN mailing_campaign_plan_recipients p
		  ON p.campaign_id = te.campaign_id AND p.subscriber_id = te.subscriber_id
		WHERE te.campaign_id = $1
		GROUP BY COALESCE(p.recipient_isp, 'unknown')
	`, campaignID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []ispEventCounts
	for rows.Next() {
		var c ispEventCounts
		if err := rows.Scan(&c.ISP, &c.Delivered, &c.Deferred, &c.Bounced, &c.Complained); err != nil {
			continue
		}
		result = append(result, c)
	}
	return result, nil
}

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

func (m *CampaignHealthMonitor) pauseCampaignISP(ctx context.Context, campaignID, isp string) {
	lowerISP := strings.ToLower(isp)

	res, err := m.db.ExecContext(ctx, `
		UPDATE mailing_campaign_isp_plans SET status = 'paused', updated_at = NOW()
		WHERE campaign_id = $1 AND LOWER(isp) = $2 AND status = 'running'
	`, campaignID, lowerISP)
	if err != nil {
		log.Printf("[HealthMonitor] pause ISP %s on campaign %s error: %v", isp, campaignID, err)
		return
	}
	affected, _ := res.RowsAffected()

	m.db.ExecContext(ctx, `
		UPDATE mailing_campaign_waves SET status = 'cancelled', updated_at = NOW()
		WHERE isp_plan_id IN (
			SELECT id FROM mailing_campaign_isp_plans
			WHERE campaign_id = $1 AND LOWER(isp) = $2
		) AND status = 'planned'
	`, campaignID, lowerISP)

	m.db.ExecContext(ctx, `
		UPDATE mailing_campaign_queue SET status = 'paused', updated_at = NOW()
		WHERE campaign_id = $1 AND LOWER(recipient_isp) = $2 AND status = 'queued'
	`, campaignID, lowerISP)

	log.Printf("[HealthMonitor] paused ISP %s on campaign %s (plans_paused=%d)", isp, campaignID, affected)

	m.db.ExecContext(ctx, `
		UPDATE mailing_campaigns
		SET pmta_config = COALESCE(pmta_config, '{}'::jsonb) ||
		    jsonb_build_object('isp_paused_' || $2,
		        jsonb_build_object('reason', 'threshold_exceeded', 'paused_at', NOW()::text)),
		    updated_at = NOW()
		WHERE id = $1
	`, campaignID, lowerISP)
}

func (m *CampaignHealthMonitor) recordListQualityMetrics() {
	ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
	defer cancel()

	rows, err := m.db.QueryContext(ctx, `
		SELECT DISTINCT c.id
		FROM mailing_campaigns c
		WHERE c.status IN ('completed', 'sent')
		  AND c.execution_mode = 'pmta_isp_wave'
		  AND c.completed_at >= NOW() - INTERVAL '2 hours'
		  AND NOT EXISTS (
			SELECT 1 FROM mailing_list_quality_metrics lqm WHERE lqm.campaign_id = c.id
		  )
	`)
	if err != nil {
		return
	}
	defer rows.Close()

	var campIDs []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			campIDs = append(campIDs, id)
		}
	}

	for _, campID := range campIDs {
		m.db.ExecContext(ctx, `
			INSERT INTO mailing_list_quality_metrics (list_id, campaign_id, total_sent, delivered, bounced, complained, acceptance_rate, complaint_rate, bounce_rate)
			SELECT
				pr.audience_source_id AS list_id,
				pr.campaign_id,
				COUNT(DISTINCT pr.subscriber_id) AS total_sent,
				COUNT(DISTINCT pr.subscriber_id) FILTER (WHERE te.event_type = 'delivered') AS delivered,
				COUNT(DISTINCT pr.subscriber_id) FILTER (WHERE te.event_type = 'bounced') AS bounced,
				COUNT(DISTINCT pr.subscriber_id) FILTER (WHERE te.event_type = 'complained') AS complained,
				CASE WHEN COUNT(DISTINCT pr.subscriber_id) > 0
					THEN COUNT(DISTINCT pr.subscriber_id) FILTER (WHERE te.event_type = 'delivered')::numeric / COUNT(DISTINCT pr.subscriber_id)
					ELSE 0 END,
				CASE WHEN COUNT(DISTINCT pr.subscriber_id) FILTER (WHERE te.event_type = 'delivered') > 0
					THEN COUNT(DISTINCT pr.subscriber_id) FILTER (WHERE te.event_type = 'complained')::numeric / COUNT(DISTINCT pr.subscriber_id) FILTER (WHERE te.event_type = 'delivered')
					ELSE 0 END,
				CASE WHEN COUNT(DISTINCT pr.subscriber_id) > 0
					THEN COUNT(DISTINCT pr.subscriber_id) FILTER (WHERE te.event_type = 'bounced')::numeric / COUNT(DISTINCT pr.subscriber_id)
					ELSE 0 END
			FROM mailing_campaign_plan_recipients pr
			LEFT JOIN mailing_tracking_events te
				ON te.campaign_id = pr.campaign_id AND te.subscriber_id = pr.subscriber_id
			WHERE pr.campaign_id = $1
			  AND pr.audience_source_type = 'list'
			  AND pr.audience_source_id IS NOT NULL
			GROUP BY pr.audience_source_id, pr.campaign_id
			HAVING COUNT(DISTINCT pr.subscriber_id) >= 10
		`, campID)
		log.Printf("[HealthMonitor] recorded list quality metrics for campaign %s", campID)
	}
}

func (m *CampaignHealthMonitor) recordCompletedMetrics() {
	ctx, cancel := context.WithTimeout(m.ctx, 15*time.Second)
	defer cancel()

	rows, err := m.db.QueryContext(ctx, `
		SELECT r.id, r.executed_campaign_id, c.status,
		       c.sent_count,
		       COALESCE(c.delivered_count, 0),
		       COALESCE(c.bounce_count, 0),
		       CASE WHEN COALESCE(c.hard_bounce_count,0)+COALESCE(c.soft_bounce_count,0)>0 THEN COALESCE(c.hard_bounce_count,0) ELSE COALESCE(c.bounce_count,0) END,
		       CASE WHEN COALESCE(c.hard_bounce_count,0)+COALESCE(c.soft_bounce_count,0)>0 THEN COALESCE(c.soft_bounce_count,0) ELSE 0 END,
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
			"sent":         sent,
			"delivered":    delivered,
			"hard_bounces": hardBounces,
			"soft_bounces": softBounces,
			"opens":        opens,
			"clicks":       clicks,
			"complaints":   complaints,
			"recorded_at":  time.Now().Format(time.RFC3339),
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

// checkLateCampaigns pages the on-call operator via SMS when a campaign is
// still stuck in 'scheduled' or 'preparing' longer than latenessThreshold
// after its scheduled_at. The CAMPAIGN status transitions to 'sending' only
// when the first wave's EnqueuePMTAWave runs (pmta_wave_dispatcher.go) —
// so a stale status here means the wave pipeline is not making progress.
//
// Dedup: late_alert_sent_at column on mailing_campaigns is stamped after a
// successful SMS. We suppress re-alerts until latenessReAlert has elapsed so
// a stuck campaign doesn't spam the pager every 60s.
func (m *CampaignHealthMonitor) checkLateCampaigns() {
	if !m.latenessEnabled || m.latenessAlerter == nil || len(m.latenessRecipients) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(m.ctx, 20*time.Second)
	defer cancel()

	thresholdSecs := int(m.latenessThreshold.Seconds())
	reAlertSecs := int(m.latenessReAlert.Seconds())

	rows, err := m.db.QueryContext(ctx, `
		SELECT id, COALESCE(name, ''), scheduled_at
		FROM mailing_campaigns
		WHERE status IN ('scheduled', 'preparing')
		  AND scheduled_at IS NOT NULL
		  AND scheduled_at < NOW() - make_interval(secs => $1)
		  AND (late_alert_sent_at IS NULL
		       OR late_alert_sent_at < NOW() - make_interval(secs => $2))
		ORDER BY scheduled_at ASC
		LIMIT 25
	`, thresholdSecs, reAlertSecs)
	if err != nil {
		if !strings.Contains(err.Error(), "does not exist") {
			log.Printf("[HealthMonitor] lateness query error: %v", err)
		}
		return
	}
	defer rows.Close()

	type lateCampaign struct {
		ID          string
		Name        string
		ScheduledAt time.Time
	}
	var late []lateCampaign
	for rows.Next() {
		var lc lateCampaign
		if err := rows.Scan(&lc.ID, &lc.Name, &lc.ScheduledAt); err != nil {
			continue
		}
		late = append(late, lc)
	}
	if err := rows.Err(); err != nil {
		log.Printf("[HealthMonitor] lateness rows error: %v", err)
	}

	for _, lc := range late {
		delay := time.Since(lc.ScheduledAt).Round(time.Minute)
		name := lc.Name
		if name == "" {
			name = lc.ID
		}
		body := fmt.Sprintf(
			"[Project Jarvis] Campaign %q did not send at scheduled time (%s UTC, +%s late). id=%s",
			truncateName(name, 48),
			lc.ScheduledAt.UTC().Format("2006-01-02 15:04"),
			delay,
			lc.ID,
		)

		var anySuccess bool
		for _, to := range m.latenessRecipients {
			sid, err := m.latenessAlerter.SendSMS(ctx, to, body)
			if err != nil {
				log.Printf("[HealthMonitor] late-alert SMS FAILED campaign=%s to=%s err=%v", lc.ID, to, err)
				continue
			}
			anySuccess = true
			log.Printf("[HealthMonitor] late-alert SMS sent campaign=%s to=%s sid=%s", lc.ID, to, sid)
		}

		// Only stamp the dedup column if at least one SMS actually went out —
		// otherwise we want to retry on the next tick.
		if anySuccess {
			if _, err := m.db.ExecContext(ctx, `
				UPDATE mailing_campaigns
				SET late_alert_sent_at = NOW(), updated_at = NOW()
				WHERE id = $1
			`, lc.ID); err != nil {
				log.Printf("[HealthMonitor] late-alert dedup stamp error campaign=%s: %v", lc.ID, err)
			}
		}
	}
}

func truncateName(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
