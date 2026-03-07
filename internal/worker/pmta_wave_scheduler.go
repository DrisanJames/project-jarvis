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
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()

	rows, err := s.db.QueryContext(ctx, `
		SELECT id
		FROM mailing_campaign_waves
		WHERE status = 'planned'
		  AND scheduled_at <= NOW()
		ORDER BY scheduled_at ASC
		LIMIT 50
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

	for _, waveID := range waveIDs {
		lock := distlock.NewLock(s.redisClient, s.db, fmt.Sprintf("pmta-wave:%s", waveID), 2*time.Minute)
		acquired, err := lock.Acquire(ctx)
		if err != nil || !acquired {
			continue
		}
		_ = s.dispatchWave(ctx, waveID)
		lock.Release(ctx)
	}
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
