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

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/pkg/distlock"
	"github.com/redis/go-redis/v9"
)

type PMTAWaveScheduler struct {
	db           *sql.DB
	redisClient  *redis.Client
	sqsClient    *sqs.Client
	queueURL     string
	pollInterval time.Duration
	ctx          context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup
	running      bool
}

func NewPMTAWaveScheduler(db *sql.DB, sqsClient *sqs.Client, queueURL string) *PMTAWaveScheduler {
	return &PMTAWaveScheduler{
		db:           db,
		sqsClient:    sqsClient,
		queueURL:     queueURL,
		pollInterval: 15 * time.Second,
	}
}

func (s *PMTAWaveScheduler) SetRedisClient(client *redis.Client) {
	s.redisClient = client
}

func (s *PMTAWaveScheduler) Start() error {
	if s.running {
		return fmt.Errorf("PMTA wave scheduler already running")
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.running = true
	s.wg.Add(1)
	go s.loop()
	return nil
}

func (s *PMTAWaveScheduler) Stop() {
	if !s.running {
		return
	}
	s.running = false
	s.cancel()
	s.wg.Wait()
}

func (s *PMTAWaveScheduler) loop() {
	defer s.wg.Done()
	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.dispatchDueWaves()
		}
	}
}

func (s *PMTAWaveScheduler) dispatchDueWaves() {
	ctx, cancel := context.WithTimeout(s.ctx, 120*time.Second)
	defer cancel()

	rows, err := s.db.QueryContext(ctx, `
		SELECT id
		FROM mailing_campaign_waves
		WHERE status = 'planned'
		  AND scheduled_at <= NOW()
		ORDER BY scheduled_at ASC
		LIMIT 100
	`)
	if err != nil {
		log.Printf("[PMTAWaveScheduler] fetch due waves: %v", err)
		return
	}
	defer rows.Close()

	var waveIDs []string
	for rows.Next() {
		var waveID uuid.UUID
		if rows.Scan(&waveID) == nil {
			waveIDs = append(waveIDs, waveID.String())
		}
	}

	if len(waveIDs) == 0 {
		return
	}
	log.Printf("[PMTAWaveScheduler] found %d due waves, processing with %d parallel workers", len(waveIDs), waveDispatchConcurrency)

	sem := make(chan struct{}, waveDispatchConcurrency)
	var wg sync.WaitGroup

	for _, wid := range waveIDs {
		if ctx.Err() != nil {
			log.Printf("[PMTAWaveScheduler] parent context expired, %d waves remaining unprocessed", len(waveIDs))
			break
		}

		sem <- struct{}{}
		wg.Add(1)
		go func(waveID string) {
			defer wg.Done()
			defer func() { <-sem }()
			s.processOneWave(ctx, waveID)
		}(wid)
	}
	wg.Wait()
}

const waveDispatchConcurrency = 5

func (s *PMTAWaveScheduler) processOneWave(parentCtx context.Context, waveID string) {
	ctx, cancel := context.WithTimeout(parentCtx, 30*time.Second)
	defer cancel()

	if blocked, reason := s.checkWaveGate(ctx, waveID); blocked {
		log.Printf("[WaveGate] wave %s blocked: %s", waveID, reason)
		return
	}

	lock := distlock.NewLock(s.redisClient, s.db, fmt.Sprintf("pmta-wave:%s", waveID), 2*time.Minute)
	acquired, err := lock.Acquire(ctx)
	if err != nil || !acquired {
		if err != nil {
			log.Printf("[PMTAWaveScheduler] lock acquire error for wave %s: %v", waveID, err)
		}
		return
	}
	enqueued, err := EnqueuePMTAWave(ctx, s.db, waveID)
	if err != nil {
		log.Printf("[PMTAWaveScheduler] enqueue error for wave %s: %v", waveID, err)
	} else {
		log.Printf("[PMTAWaveScheduler] wave %s enqueued %d recipients", waveID, enqueued)
	}
	lock.Release(ctx)
}

type waveGatingConfig struct {
	DependsOnCampaignID string  `json:"depends_on_campaign_id"`
	MinAcceptanceRate   float64 `json:"min_acceptance_rate"`
	GateDeadlineUTC     string  `json:"gate_deadline_utc"`
}

func (s *PMTAWaveScheduler) checkWaveGate(ctx context.Context, waveID string) (blocked bool, reason string) {
	var campaignID, ispPlanID string
	err := s.db.QueryRowContext(ctx, `
		SELECT w.campaign_id, w.isp_plan_id
		FROM mailing_campaign_waves w
		WHERE w.id = $1
	`, waveID).Scan(&campaignID, &ispPlanID)
	if err != nil {
		return false, ""
	}

	var pmtaConfigRaw sql.NullString
	err = s.db.QueryRowContext(ctx, `
		SELECT pmta_config::text
		FROM mailing_campaigns
		WHERE id = $1 AND pmta_config IS NOT NULL
	`, campaignID).Scan(&pmtaConfigRaw)
	if err != nil || !pmtaConfigRaw.Valid {
		return false, ""
	}

	var cfg struct {
		CampaignInput struct {
			WaveGating *waveGatingConfig `json:"wave_gating"`
		} `json:"campaign_input"`
	}
	if err := json.Unmarshal([]byte(pmtaConfigRaw.String), &cfg); err != nil || cfg.CampaignInput.WaveGating == nil {
		return false, ""
	}

	gate := cfg.CampaignInput.WaveGating
	if gate.DependsOnCampaignID == "" {
		return false, ""
	}

	var waveISP string
	err = s.db.QueryRowContext(ctx, `
		SELECT LOWER(isp) FROM mailing_campaign_isp_plans WHERE id = $1
	`, ispPlanID).Scan(&waveISP)
	if err != nil {
		return false, ""
	}

	pastDeadline := false
	if gate.GateDeadlineUTC != "" {
		deadline, err := time.Parse(time.RFC3339, gate.GateDeadlineUTC)
		if err == nil && time.Now().UTC().After(deadline) {
			pastDeadline = true
		}
	}

	// Per-ISP acceptance: join with plan recipients so we evaluate the
	// dependent campaign's performance for THIS wave's ISP, not globally.
	var delivered, total int
	err = s.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE te.event_type = 'delivered'),
			COUNT(*) FILTER (WHERE te.event_type IN ('delivered', 'bounced', 'deferred'))
		FROM mailing_tracking_events te
		JOIN mailing_campaign_plan_recipients pr
		  ON pr.campaign_id = te.campaign_id AND pr.subscriber_id = te.subscriber_id
		WHERE te.campaign_id = $1
		  AND LOWER(pr.recipient_isp) = $2
		  AND te.created_at >= NOW() - INTERVAL '12 hours'
	`, gate.DependsOnCampaignID, waveISP).Scan(&delivered, &total)
	if err != nil || total < 20 {
		if pastDeadline {
			log.Printf("[WaveGate] ISP %s RELEASED for campaign %s (insufficient data total=%d, past deadline — no negative signal, allowing)",
				waveISP, campaignID, total)
			return false, ""
		}
		return true, fmt.Sprintf("insufficient per-ISP data for dependent campaign ISP %s (total=%d, need 20+), held", waveISP, total)
	}

	acceptanceRate := float64(delivered) / float64(total)
	minRate := gate.MinAcceptanceRate
	if minRate <= 0 {
		minRate = 0.90
	}
	if minRate > 1.0 {
		log.Printf("[WaveGate] WARNING: min_acceptance_rate=%.2f looks like a percentage, converting to ratio", minRate)
		minRate = minRate / 100.0
	}

	if acceptanceRate < minRate {
		if pastDeadline {
			s.db.ExecContext(ctx, `
				UPDATE mailing_campaign_waves SET status = 'cancelled', updated_at = NOW()
				WHERE id = $1 AND status = 'planned'
			`, waveID)
			return true, fmt.Sprintf("dependent campaign ISP %s acceptance %.1f%% < %.1f%% threshold (past deadline), cancelled",
				waveISP, acceptanceRate*100, minRate*100)
		}
		return true, fmt.Sprintf("dependent campaign ISP %s acceptance %.1f%% < %.1f%% threshold, held",
			waveISP, acceptanceRate*100, minRate*100)
	}

	log.Printf("[WaveGate] ISP %s CLEARED for campaign %s (acceptance=%.1f%%, threshold=%.1f%%)",
		waveISP, campaignID, acceptanceRate*100, minRate*100)
	return false, ""
}

func (s *PMTAWaveScheduler) dispatchWave(ctx context.Context, waveID string) error {
	if s.sqsClient == nil || strings.TrimSpace(s.queueURL) == "" {
		_, err := EnqueuePMTAWave(ctx, s.db, waveID)
		return err
	}

	var campaignID, planID uuid.UUID
	var idempotencyKey string
	if err := s.db.QueryRowContext(ctx, `
		SELECT campaign_id, isp_plan_id, idempotency_key
		FROM mailing_campaign_waves
		WHERE id = $1
		  AND status = 'planned'
	`, waveID).Scan(&campaignID, &planID, &idempotencyKey); err != nil {
		return err
	}

	payload := PMTAWaveMessage{
		WaveID:         waveID,
		CampaignID:     campaignID.String(),
		ISPPlanID:      planID.String(),
		IdempotencyKey: idempotencyKey,
	}
	body, _ := json.Marshal(payload)
	input := &sqs.SendMessageInput{
		QueueUrl:    aws.String(s.queueURL),
		MessageBody: aws.String(string(body)),
	}
	if strings.HasSuffix(s.queueURL, ".fifo") {
		input.MessageGroupId = aws.String(planID.String())
		input.MessageDeduplicationId = aws.String(idempotencyKey)
	}
	out, err := s.sqsClient.SendMessage(ctx, input)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE mailing_campaign_waves
		SET status = 'dispatched', sqs_message_id = $2, updated_at = NOW()
		WHERE id = $1 AND status = 'planned'
	`, waveID, aws.ToString(out.MessageId))
	return err
}
