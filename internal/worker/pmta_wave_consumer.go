package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/ignite/sparkpost-monitor/internal/mailing"
)

type PMTAWaveMessage struct {
	WaveID         string `json:"wave_id"`
	CampaignID     string `json:"campaign_id"`
	ISPPlanID      string `json:"isp_plan_id"`
	Attempt        int    `json:"attempt,omitempty"`
	IdempotencyKey string `json:"idempotency_key"`
}

type PMTAWaveConsumer struct {
	sqsClient  *sqs.Client
	queueURL   string
	db         *sql.DB
	capChecker *mailing.CapChecker
	// familyGovernor: yahoo-family daily ceiling (family_governor.go); nil/off = no effect.
	familyGovernor *FamilyGovernor
	done           chan struct{}
}

func NewPMTAWaveConsumer(sqsClient *sqs.Client, queueURL string, db *sql.DB) *PMTAWaveConsumer {
	return &PMTAWaveConsumer{
		sqsClient: sqsClient,
		queueURL:  queueURL,
		db:        db,
		done:      make(chan struct{}),
	}
}

// SetCapChecker injects the cross-brand cap checker so the SQS-driven
// wave path stays consistent with the polling scheduler. Pass nil to keep
// the legacy single-status claim path.
func (c *PMTAWaveConsumer) SetCapChecker(cc *mailing.CapChecker) {
	c.capChecker = cc
}

// SetFamilyGovernor mirrors PMTAWaveScheduler.SetFamilyGovernor for the
// SQS-driven path so both dispatch entrypoints see the same governor.
func (c *PMTAWaveConsumer) SetFamilyGovernor(g *FamilyGovernor) {
	c.familyGovernor = g
}

func (c *PMTAWaveConsumer) Start(ctx context.Context) {
	log.Printf("PMTA wave consumer started (queue=%s)", c.queueURL)
	go c.poll(ctx)
}

func (c *PMTAWaveConsumer) Stop() {
	close(c.done)
}

func (c *PMTAWaveConsumer) poll(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.done:
			return
		default:
		}

		out, err := c.sqsClient.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:            aws.String(c.queueURL),
			MaxNumberOfMessages: 10,
			WaitTimeSeconds:     20,
		})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("PMTA wave SQS receive error: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}

		for _, msg := range out.Messages {
			if processWaveMessage(ctx, c.db, aws.ToString(msg.Body), c.capChecker, c.familyGovernor) {
				c.deleteMessage(ctx, msg.ReceiptHandle)
			}
		}
	}
}

// processWaveMessage handles a single SQS message body. Returns true if the
// message should be deleted (successful processing or unrecoverable payload).
func processWaveMessage(ctx context.Context, db *sql.DB, body string, capChecker *mailing.CapChecker, gov *FamilyGovernor) bool {
	var payload PMTAWaveMessage
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		log.Printf("PMTA wave bad message: %v", err)
		return true
	}
	if _, err := enqueuePMTAWave(ctx, db, payload.WaveID, capChecker, gov); err != nil {
		log.Printf("PMTA wave enqueue error (%s): %v", payload.WaveID, err)
		return false
	}
	return true
}

func (c *PMTAWaveConsumer) deleteMessage(ctx context.Context, handle *string) {
	_, _ = c.sqsClient.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(c.queueURL),
		ReceiptHandle: handle,
	})
}
