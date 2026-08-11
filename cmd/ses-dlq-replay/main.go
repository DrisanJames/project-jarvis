// Command ses-dlq-replay drains the SES event dead-letter queue back into the
// webhook.
//
// WHY THIS EXISTS
// ---------------
// SES publishes every Send/Delivery/Open/Click/Bounce/Complaint through an SNS
// HTTPS subscription. That subscription allows only 3 retries over ~60s, so any
// notification the server cannot answer in time used to be DESTROYED — 13,078
// of them in the 24h to 2026-08-11, including roughly 1,800 open/click events.
// SES never re-emits, so there was no way to get them back.
//
// The subscription now has a RedrivePolicy pointing at
// arn:aws:sqs:us-west-1:146361001621:ses-events-prod-dlq (14-day retention), so
// undeliverable notifications are CAPTURED instead of dropped. This tool is the
// other half: it reads them back out and re-POSTs them to the webhook, which
// persists them exactly as if SNS had delivered them the first time.
//
// Replay is SAFE to run repeatedly. Every tracking-event id is a deterministic
// SHA1 of (campaign, send id, event type, event timestamp) and the INSERT is
// ON CONFLICT DO NOTHING, so a message that was in fact already persisted is a
// no-op rather than a double count. Event timestamps come from the SES payload,
// not from replay time, so late replay does not distort the hourly series.
//
// DRY BY DEFAULT — pass --confirm to actually POST and delete.
//
//	go run ./cmd/ses-dlq-replay                 # report what is waiting
//	go run ./cmd/ses-dlq-replay --confirm       # replay it
package main

import (
	"bytes"
	"context"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

const (
	defaultQueueURL = "https://sqs.us-west-1.amazonaws.com/146361001621/ses-events-prod-dlq"
	defaultRegion   = "us-west-1"
	defaultEndpoint = "https://projectjarvis.io/api/mailing/webhooks/ses-events"
)

func main() {
	var (
		confirm   = flag.Bool("confirm", false, "actually replay and delete (default: dry run)")
		queueURL  = flag.String("queue", envOr("SES_DLQ_URL", defaultQueueURL), "DLQ URL")
		region    = flag.String("region", envOr("SES_DLQ_REGION", defaultRegion), "DLQ region")
		endpoint  = flag.String("endpoint", envOr("SES_WEBHOOK_URL", defaultEndpoint), "webhook endpoint")
		maxMsgs   = flag.Int("max", 0, "stop after N messages (0 = drain everything)")
		ratePerS  = flag.Int("rate", 20, "max messages replayed per second")
		timeoutMin = flag.Int("timeout-min", 30, "give up after N minutes")
	)
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutMin)*time.Minute)
	defer cancel()

	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(*region))
	if err != nil {
		log.Fatalf("aws config: %v", err)
	}
	client := sqs.NewFromConfig(cfg)

	// Report depth first so a dry run is actually informative.
	attrs, err := client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       queueURL,
		AttributeNames: []sqstypes.QueueAttributeName{"ApproximateNumberOfMessages"},
	})
	if err != nil {
		log.Fatalf("get queue attributes: %v", err)
	}
	depth := attrs.Attributes["ApproximateNumberOfMessages"]
	log.Printf("[ses-dlq-replay] queue=%s approx_depth=%s endpoint=%s confirm=%v",
		*queueURL, depth, *endpoint, *confirm)

	if depth == "0" {
		log.Printf("[ses-dlq-replay] nothing to replay — no SES events were dead-lettered")
		return
	}
	if !*confirm {
		log.Printf("[ses-dlq-replay] DRY RUN — %s message(s) waiting. Re-run with --confirm to replay them.", depth)
		return
	}

	httpClient := &http.Client{Timeout: 30 * time.Second}
	ticker := time.NewTicker(time.Second / time.Duration(max(1, *ratePerS)))
	defer ticker.Stop()

	var replayed, failed, empties int
	for {
		if *maxMsgs > 0 && replayed >= *maxMsgs {
			log.Printf("[ses-dlq-replay] reached --max=%d", *maxMsgs)
			break
		}
		if ctx.Err() != nil {
			log.Printf("[ses-dlq-replay] timeout reached")
			break
		}

		out, err := client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:            queueURL,
			MaxNumberOfMessages: 10,
			WaitTimeSeconds:     5,
			VisibilityTimeout:   60,
		})
		if err != nil {
			log.Printf("[ses-dlq-replay] receive error: %v", err)
			break
		}
		if len(out.Messages) == 0 {
			empties++
			// Two consecutive empty long-polls means the queue is drained.
			if empties >= 2 {
				break
			}
			continue
		}
		empties = 0

		for _, m := range out.Messages {
			select {
			case <-ticker.C:
			case <-ctx.Done():
			}
			if m.Body == nil {
				continue
			}
			if replaySingle(ctx, httpClient, *endpoint, *m.Body) {
				// Only delete once the webhook has accepted it. A message left
				// on the queue is retried next run; a message deleted after a
				// failure is gone for good — the exact mistake this whole
				// exercise exists to correct.
				if _, derr := client.DeleteMessage(ctx, &sqs.DeleteMessageInput{
					QueueUrl:      queueURL,
					ReceiptHandle: m.ReceiptHandle,
				}); derr != nil {
					log.Printf("[ses-dlq-replay] delete failed (will reappear): %v", derr)
				}
				replayed++
			} else {
				failed++
			}
		}
		log.Printf("[ses-dlq-replay] progress replayed=%d failed=%d", replayed, failed)
	}

	log.Printf("[ses-dlq-replay] DONE replayed=%d failed=%d", replayed, failed)
	if failed > 0 {
		// Non-zero exit so a cron/CI wrapper notices.
		os.Exit(1)
	}
}

// replaySingle POSTs one raw SNS notification body back to the webhook. The
// body still carries its original SNS Signature, so the endpoint's signature
// verification passes exactly as it would for a live delivery.
func replaySingle(ctx context.Context, c *http.Client, endpoint, body string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader([]byte(body)))
	if err != nil {
		log.Printf("[ses-dlq-replay] build request: %v", err)
		return false
	}
	req.Header.Set("Content-Type", "text/plain; charset=UTF-8")
	req.Header.Set("x-amz-sns-message-type", "Notification")

	resp, err := c.Do(req)
	if err != nil {
		log.Printf("[ses-dlq-replay] POST error: %v", err)
		return false
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return true
	}
	log.Printf("[ses-dlq-replay] POST returned HTTP %d — leaving message on the queue", resp.StatusCode)
	return false
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

