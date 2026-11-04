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

	"github.com/lib/pq"
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

	// checkStalledDrain dedup: campaign id → last alert time (in-memory,
	// 1h cooldown). Only touched from the monitor loop goroutine.
	drainAlertedAt map[string]time.Time
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
		reAlert = 24 * time.Hour
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
	var tick int
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			tick++
			m.checkCampaigns()
			m.checkISPThresholds()
			m.recordCompletedMetrics()
			m.recordListQualityMetrics()
			m.checkLateCampaigns()
			// Every stalledDrainEveryTicks ticks (5 min at the 60s interval):
			// the drain probe reads the queue on a saturated primary from
			// every replica, and a 5-min resolution is enough for an alert
			// whose criterion is already 30 min stale.
			if tick%stalledDrainEveryTicks == 0 {
				m.checkStalledDrain()
			}
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

// checkLateCampaigns pages the on-call operator when campaigns are still
// stuck in 'scheduled' or 'preparing' longer than latenessThreshold after their
// scheduled_at. The CAMPAIGN status transitions to 'sending' only when the
// first wave's EnqueuePMTAWave runs (pmta_wave_dispatcher.go) — so a stale
// status here means the wave pipeline is not making progress.
//
// Roll-up: ONE message per tick, "N campaigns late · worst +Xm", with the top
// lateLimitBodyLines campaigns by delay as body lines. N is counted separately
// so the headline is true even when more than 25 are late.
//
// Dedup: late_alert_sent_at on mailing_campaigns is stamped (one UPDATE for
// every reported id) after a successful post. Re-alerts are suppressed until
// latenessReAlert has elapsed. Campaigns beyond the LIMIT 25 select are not
// stamped, so they surface in the next tick's roll-up.
func (m *CampaignHealthMonitor) checkLateCampaigns() {
	if !m.latenessEnabled || m.latenessAlerter == nil || len(m.latenessRecipients) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(m.ctx, 20*time.Second)
	defer cancel()

	thresholdSecs := int(m.latenessThreshold.Seconds())
	reAlertSecs := int(m.latenessReAlert.Seconds())

	const latePredicate = `
		FROM mailing_campaigns
		WHERE status IN ('scheduled', 'preparing')
		  AND scheduled_at IS NOT NULL
		  AND scheduled_at < NOW() - make_interval(secs => $1)
		  AND (late_alert_sent_at IS NULL
		       OR late_alert_sent_at < NOW() - make_interval(secs => $2))`

	rows, err := m.db.QueryContext(ctx, `
		SELECT id, COALESCE(name, ''), scheduled_at`+latePredicate+`
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
	rows.Close()
	if len(late) == 0 {
		return
	}

	total := len(late)
	if err := m.db.QueryRowContext(ctx, `SELECT COUNT(*)`+latePredicate, thresholdSecs, reAlertSecs).Scan(&total); err != nil {
		log.Printf("[HealthMonitor] lateness count error (headline uses %d): %v", len(late), err)
		total = len(late)
	}
	if total < len(late) {
		total = len(late)
	}

	// Ordered by scheduled_at ASC, so late[0] is the worst delay.
	noun := "campaigns"
	if total == 1 {
		noun = "campaign"
	}
	// Standard shape (docs/SLACK_MESSAGE_STANDARD.md): first line is the
	// headline, the rest is the body. SlackAlerter splits on the first "\n".
	var b strings.Builder
	fmt.Fprintf(&b, "%d %s late · worst +%dm", total, noun, minutesSince(late[0].ScheduledAt))
	ids := make([]string, 0, len(late))
	for i, lc := range late {
		ids = append(ids, lc.ID)
		if i >= lateLimitBodyLines {
			continue
		}
		name := lc.Name
		if name == "" {
			name = lc.ID
		}
		fmt.Fprintf(&b, "\n`%s` · +%dm", truncateName(name, 48), minutesSince(lc.ScheduledAt))
	}
	body := b.String()

	var anySuccess bool
	for _, to := range m.latenessRecipients {
		sid, err := m.latenessAlerter.SendSMS(ctx, to, body)
		if err != nil {
			log.Printf("[HealthMonitor] late-alert FAILED campaigns=%d to=%s err=%v", total, to, err)
			continue
		}
		anySuccess = true
		log.Printf("[HealthMonitor] late-alert sent campaigns=%d reported=%d to=%s sid=%s", total, len(ids), to, sid)
	}

	// Only stamp the dedup column if at least one post actually went out —
	// otherwise we want to retry on the next tick.
	if anySuccess {
		if _, err := m.db.ExecContext(ctx, `
			UPDATE mailing_campaigns
			SET late_alert_sent_at = NOW(), updated_at = NOW()
			WHERE id = ANY($1::uuid[])
		`, pq.Array(ids)); err != nil {
			log.Printf("[HealthMonitor] late-alert dedup stamp error campaigns=%d: %v", len(ids), err)
		}
	}
}

// lateLimitBodyLines caps the per-campaign body lines of a roll-up alert
// (SLACK_MESSAGE_STANDARD: ≤6 body lines).
const lateLimitBodyLines = 5

// stalledDrainCooldown is how long a campaign stays muted after it has been
// reported by checkStalledDrain.
const stalledDrainCooldown = time.Hour

// stalledDrainLimit caps the rows checkStalledDrain reads per tick.
const stalledDrainLimit = 25

// stalledDrainEveryTicks: checkStalledDrain runs on every Nth monitor tick.
const stalledDrainEveryTicks = 5

// stalledDrainFrom is shared by the row select and the companion aggregate.
// Driven from the queue side: the partial index
// idx_queue_campaign_status_scheduled (WHERE status='queued') yields the
// overdue queued rows, and idx_waves_active_by_campaign answers the
// planned-wave probe — no scan of every wave of every sending campaign (the
// MAX(window_end_at) shape timed out at 120s on prod, 2026-11-04).
// $1 = campaign ids still inside the alert cooldown (excluded).
const stalledDrainFrom = `
		WITH q AS (
			SELECT campaign_id, COUNT(*) AS queued, MIN(scheduled_at) AS oldest_due
			FROM mailing_campaign_queue
			WHERE status = 'queued'
			  AND scheduled_at < NOW() - interval '30 minutes'
			GROUP BY campaign_id
		)
		FROM q
		JOIN mailing_campaigns c ON c.id = q.campaign_id
		WHERE c.status = 'sending'
		  AND c.scheduled_at > NOW() - interval '2 days'
		  AND NOT (c.id = ANY($1::uuid[]))
		  AND NOT EXISTS (
			SELECT 1 FROM mailing_campaign_waves wv
			WHERE wv.campaign_id = c.id AND wv.status = 'planned'
		  )`

// checkStalledDrain alerts on queued rows >30 min past their own scheduled_at
// on a sending campaign (scheduled within the last 2 days) with no planned
// waves left. It does NOT read wave window_end_at — "past window" means the
// queue rows' own scheduled_at. The 2-day age bound keeps a zombie 'sending'
// campaign with old queued rows from paging hourly forever. ONE roll-up
// message per run (every stalledDrainEveryTicks ticks):
// "N campaigns past window still draining · M recipients · oldest due HH:MM UTC".
//
// Dedup is an in-memory map (campaign id → last alert time) with a 1h
// cooldown; no schema change. Muted ids are excluded in SQL so N, M and the
// LIMIT cover only reportable campaigns. Trade-off: the map is per process.
// A restart forgets it, so a campaign can be reported again — at most one
// duplicate per boot per campaign, which is accepted. The monitor is not
// leader-gated (cmd/server/main.go starts it on every task), so each running
// replica keeps its own map and posts its own roll-up: R replicas → up to R
// posts per campaign per cooldown window.
func (m *CampaignHealthMonitor) checkStalledDrain() {
	if !m.latenessEnabled || m.latenessAlerter == nil || len(m.latenessRecipients) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(m.ctx, 20*time.Second)
	defer cancel()

	now := time.Now()
	if m.drainAlertedAt == nil {
		m.drainAlertedAt = make(map[string]time.Time)
	}
	muted := make([]string, 0, len(m.drainAlertedAt))
	for id, at := range m.drainAlertedAt {
		if now.Sub(at) >= stalledDrainCooldown {
			delete(m.drainAlertedAt, id)
			continue
		}
		muted = append(muted, id)
	}

	rows, err := m.db.QueryContext(ctx, `
		SELECT c.id, COALESCE(c.name, ''), q.queued, q.oldest_due`+stalledDrainFrom+`
		ORDER BY q.oldest_due ASC
		LIMIT `+fmt.Sprint(stalledDrainLimit), pq.Array(muted))
	if err != nil {
		if !strings.Contains(err.Error(), "does not exist") {
			log.Printf("[HealthMonitor] stalled-drain query error: %v", err)
		}
		return
	}
	defer rows.Close()

	type drainCampaign struct {
		ID        string
		Name      string
		Queued    int64
		OldestDue time.Time
	}
	var stalled []drainCampaign
	var recipients int64
	for rows.Next() {
		var dc drainCampaign
		if err := rows.Scan(&dc.ID, &dc.Name, &dc.Queued, &dc.OldestDue); err != nil {
			continue
		}
		stalled = append(stalled, dc)
		recipients += dc.Queued
	}
	if err := rows.Err(); err != nil {
		log.Printf("[HealthMonitor] stalled-drain rows error: %v", err)
	}
	rows.Close()
	if len(stalled) == 0 {
		return
	}

	// N and M come from the returned rows unless the LIMIT was hit; only then
	// pay for the companion aggregate over the same CTE.
	total := len(stalled)
	if len(stalled) >= stalledDrainLimit {
		var n, sum int64
		if err := m.db.QueryRowContext(ctx, `
			SELECT COUNT(*), COALESCE(SUM(q.queued), 0)`+stalledDrainFrom, pq.Array(muted)).Scan(&n, &sum); err != nil {
			log.Printf("[HealthMonitor] stalled-drain count error (headline uses %d rows): %v", len(stalled), err)
		} else if int(n) >= total {
			total, recipients = int(n), sum
		}
	}

	noun := "campaigns"
	if total == 1 {
		noun = "campaign"
	}
	// Ordered by oldest_due ASC, so stalled[0] holds the oldest due row.
	var b strings.Builder
	fmt.Fprintf(&b, "%d %s past window still draining · %d recipients · oldest due %s UTC",
		total, noun, recipients, stalled[0].OldestDue.UTC().Format("15:04"))
	for i, dc := range stalled {
		if i >= lateLimitBodyLines {
			break
		}
		name := dc.Name
		if name == "" {
			name = dc.ID
		}
		fmt.Fprintf(&b, "\n`%s` · %d queued · oldest due %s UTC",
			truncateName(name, 48), dc.Queued, dc.OldestDue.UTC().Format("15:04"))
	}
	body := b.String()

	var anySuccess bool
	for _, to := range m.latenessRecipients {
		sid, err := m.latenessAlerter.SendSMS(ctx, to, body)
		if err != nil {
			log.Printf("[HealthMonitor] stalled-drain alert FAILED campaigns=%d to=%s err=%v", total, to, err)
			continue
		}
		anySuccess = true
		log.Printf("[HealthMonitor] stalled-drain alert sent campaigns=%d recipients=%d to=%s sid=%s", total, recipients, to, sid)
	}
	// Mute only after a successful post — otherwise retry next tick. Only the
	// returned rows are muted; campaigns beyond the LIMIT surface next tick.
	if anySuccess {
		for _, dc := range stalled {
			m.drainAlertedAt[dc.ID] = now
		}
	}
}

func minutesSince(t time.Time) int {
	return int(time.Since(t).Round(time.Minute) / time.Minute)
}

func truncateName(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
