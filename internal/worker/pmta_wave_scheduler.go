package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/mailing"
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

	// capChecker is wired in main.go alongside the SendWorkerPool's
	// CapChecker so the dispatcher can Peek the cap and substitute a
	// reserve subscriber for any capped row. Nil disables the cap-aware
	// claim path (legacy single-status SELECT). Slice 4 of cap-aware
	// reserve pool plan.
	capChecker *mailing.CapChecker
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

// SetCapChecker injects the cross-brand cap checker so the dispatcher can
// Peek subscribers before queueing them. Pass nil (or omit the call) to
// keep the legacy single-status claim path.
func (s *PMTAWaveScheduler) SetCapChecker(cc *mailing.CapChecker) {
	s.capChecker = cc
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

	useLegacy := os.Getenv("DISABLE_DISPATCHER_FAIRNESS") == "true"

	var (
		rows *sql.Rows
		err  error
	)
	if useLegacy {
		log.Printf("[PMTAWaveScheduler] DISABLE_DISPATCHER_FAIRNESS=true, using legacy FIFO")
		rows, err = s.db.QueryContext(ctx, `
			SELECT id
			FROM mailing_campaign_waves
			WHERE status = 'planned'
			  AND scheduled_at <= NOW()
			ORDER BY scheduled_at ASC
			LIMIT 100
		`)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			WITH ranked AS (
				SELECT
					w.id,
					sp.sending_domain,
					ROW_NUMBER() OVER (PARTITION BY sp.sending_domain ORDER BY w.scheduled_at ASC) AS domain_rank,
					w.scheduled_at
				FROM mailing_campaign_waves w
				JOIN mailing_campaigns c ON c.id = w.campaign_id
				JOIN mailing_sending_profiles sp ON sp.id = c.sending_profile_id
				WHERE w.status = 'planned'
				  AND w.scheduled_at <= NOW()
			)
			SELECT id, sending_domain
			  FROM ranked
			 WHERE domain_rank <= $1
			 ORDER BY domain_rank ASC, scheduled_at ASC
			 LIMIT $2
		`, maxWavesPerDomainPerTick, dispatchBatchSize)
	}
	if err != nil {
		log.Printf("[PMTAWaveScheduler] fetch due waves: %v", err)
		return
	}
	defer rows.Close()

	var waveIDs []string
	domainCounts := make(map[string]int)
	for rows.Next() {
		var waveID uuid.UUID
		if useLegacy {
			if scanErr := rows.Scan(&waveID); scanErr == nil {
				waveIDs = append(waveIDs, waveID.String())
			}
			continue
		}
		var sendingDomain sql.NullString
		if scanErr := rows.Scan(&waveID, &sendingDomain); scanErr == nil {
			waveIDs = append(waveIDs, waveID.String())
			key := sendingDomain.String
			if !sendingDomain.Valid || key == "" {
				key = "unknown"
			}
			domainCounts[key]++
		}
	}

	if len(waveIDs) == 0 {
		return
	}

	if useLegacy {
		log.Printf("[PMTAWaveScheduler] found %d due waves, processing with %d parallel workers", len(waveIDs), waveDispatchConcurrency)
	} else {
		log.Printf("[PMTAWaveScheduler] found %d due waves across %d domains (%s), processing with %d parallel workers",
			len(waveIDs), len(domainCounts), formatDomainCounts(domainCounts), waveDispatchConcurrency)
	}

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

// formatDomainCounts renders a map of sending_domain -> wave count as
// "<short>=<n>" tokens, sorted alphabetically by full domain for stable log
// output. The "short" form uses the leading subdomain label when present
// (e.g. "em.discountblog.com" -> "em") so the dispatch log stays readable
// when many domains are active. If the leading label is one of the common
// host prefixes ("em", "m", "mail", "news"), the next label is used to
// disambiguate sister domains.
func formatDomainCounts(counts map[string]int) string {
	if len(counts) == 0 {
		return ""
	}
	domains := make([]string, 0, len(counts))
	for d := range counts {
		domains = append(domains, d)
	}
	sort.Strings(domains)
	parts := make([]string, 0, len(domains))
	for _, d := range domains {
		parts = append(parts, fmt.Sprintf("%s=%d", shortDomainTag(d), counts[d]))
	}
	return strings.Join(parts, " ")
}

func shortDomainTag(domain string) string {
	if domain == "" {
		return "unknown"
	}
	labels := strings.Split(domain, ".")
	if len(labels) == 0 {
		return domain
	}
	first := labels[0]
	switch first {
	case "em", "m", "mail", "news":
		if len(labels) >= 2 {
			return labels[1]
		}
	}
	return first
}

const (
	waveDispatchConcurrency  = 5
	maxWavesPerDomainPerTick = 25
	dispatchBatchSize        = 100
)

func (s *PMTAWaveScheduler) processOneWave(parentCtx context.Context, waveID string) {
	ctx, cancel := context.WithTimeout(parentCtx, 30*time.Second)
	defer cancel()

	lock := distlock.NewLock(s.redisClient, s.db, fmt.Sprintf("pmta-wave:%s", waveID), 2*time.Minute)
	acquired, err := lock.Acquire(ctx)
	if err != nil || !acquired {
		if err != nil {
			log.Printf("[PMTAWaveScheduler] lock acquire error for wave %s: %v", waveID, err)
		}
		return
	}
	enqueued, err := EnqueuePMTAWave(ctx, s.db, waveID, s.capChecker)
	if err != nil {
		log.Printf("[PMTAWaveScheduler] enqueue error for wave %s: %v", waveID, err)
	} else {
		log.Printf("[PMTAWaveScheduler] wave %s enqueued %d recipients", waveID, enqueued)
	}
	lock.Release(ctx)
}

func (s *PMTAWaveScheduler) dispatchWave(ctx context.Context, waveID string) error {
	if s.sqsClient == nil || strings.TrimSpace(s.queueURL) == "" {
		_, err := EnqueuePMTAWave(ctx, s.db, waveID, s.capChecker)
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
