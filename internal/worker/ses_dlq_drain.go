package worker

// SESDLQDrainWorker (operator 2026-09-13: "close it out fully, I do not want
// to leave it lingering"). The SNS subscription that delivers SES events to
// /api/mailing/webhooks/ses-events dead-letters into ses-events-prod-dlq
// (us-west-1) whenever the webhook is unreachable — most often the ECS
// readiness window during a deploy, and bursts. 72,082 notifications sat
// there on 2026-09-13 with a 14-day expiry and nothing draining them.
//
// Every 5 minutes, under a distlock lease, this worker long-polls the DLQ and
// replays each message by POSTing the intact SNS envelope to the LOCAL
// webhook (loopback) — exactly the delivery SNS failed to make. The handler
// verifies the SNS signature, stamps event_at from the SES event's own
// timestamp and dedups on (id, event_at), so a replay lands on the original
// day and cannot double-count. A message is deleted ONLY on HTTP 2xx; any
// other outcome leaves it to reappear after the visibility timeout.
//
// Heartbeat 'ses_dlq_drain' carries the queue depth after the pass; a depth
// that is still > SES_DLQ_ALERT_DEPTH after draining reports status=error so
// WorkerHealthMonitor posts it to Slack. Kill switch SES_DLQ_DRAIN_DISABLED.
// Env: SES_DLQ_URL (default the prod DLQ), SES_DLQ_REGION (us-west-1),
// SES_DLQ_WEBHOOK_URL (default http://127.0.0.1:<port>/api/mailing/webhooks/
// ses-events), SES_DLQ_MAX_PER_TICK (default 20000).
//
// IAM: apex-ecs-task-role needs sqs:ReceiveMessage/DeleteMessage/
// DeleteMessageBatch/GetQueueAttributes on the DLQ ARN (inline policy
// sqs-ses-dlq-access, added 2026-09-13).

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/redis/go-redis/v9"

	"github.com/ignite/sparkpost-monitor/internal/pkg/distlock"
)

const (
	sesDLQWorkerName        = "ses_dlq_drain"
	sesDLQLockKey           = "ses_dlq_drain"
	DefaultSESDLQInterval   = 5 * time.Minute
	sesDLQDefaultURL        = "https://sqs.us-west-1.amazonaws.com/146361001621/ses-events-prod-dlq"
	sesDLQDefaultRegion     = "us-west-1"
	sesDLQDefaultMaxPerTick = 20000
	sesDLQDefaultAlertDepth = 1000
	sesDLQVisibilitySeconds = 120
)

// SESDLQClient is the seam over SQS (tests inject a fake).
type SESDLQClient interface {
	ReceiveMessage(ctx context.Context, in *sqs.ReceiveMessageInput, opts ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessageBatch(ctx context.Context, in *sqs.DeleteMessageBatchInput, opts ...func(*sqs.Options)) (*sqs.DeleteMessageBatchOutput, error)
	GetQueueAttributes(ctx context.Context, in *sqs.GetQueueAttributesInput, opts ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error)
}

func sesDLQDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SES_DLQ_DRAIN_DISABLED"))) {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}

type SESDLQDrainWorker struct {
	db         *sql.DB
	redis      *redis.Client
	interval   time.Duration
	queueURL   string
	region     string
	webhookURL string
	maxPerTick int
	alertDepth int
	client     SESDLQClient
	http       *http.Client
}

func NewSESDLQDrainWorker(db *sql.DB, redisClient *redis.Client, port int) *SESDLQDrainWorker {
	env := func(k, d string) string {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
		return d
	}
	w := &SESDLQDrainWorker{db: db, redis: redisClient, interval: DefaultSESDLQInterval,
		queueURL:   env("SES_DLQ_URL", sesDLQDefaultURL),
		region:     env("SES_DLQ_REGION", sesDLQDefaultRegion),
		webhookURL: env("SES_DLQ_WEBHOOK_URL", fmt.Sprintf("http://127.0.0.1:%d/api/mailing/webhooks/ses-events", port)),
		maxPerTick: sesDLQDefaultMaxPerTick, alertDepth: sesDLQDefaultAlertDepth,
		http: &http.Client{Timeout: 30 * time.Second}}
	if n, err := strconv.Atoi(env("SES_DLQ_MAX_PER_TICK", "")); err == nil && n > 0 {
		w.maxPerTick = n
	}
	if n, err := strconv.Atoi(env("SES_DLQ_ALERT_DEPTH", "")); err == nil && n >= 0 {
		w.alertDepth = n
	}
	return w
}

// SetClient injects the SQS seam (tests). Call before Start.
func (w *SESDLQDrainWorker) SetClient(c SESDLQClient) *SESDLQDrainWorker { w.client = c; return w }

// SetHTTPClient injects the replay transport (tests).
func (w *SESDLQDrainWorker) SetHTTPClient(c *http.Client) *SESDLQDrainWorker { w.http = c; return w }

func (w *SESDLQDrainWorker) WithInterval(d time.Duration) *SESDLQDrainWorker {
	if d > 0 {
		w.interval = d
	}
	return w
}

func (w *SESDLQDrainWorker) ensureClient(ctx context.Context) (SESDLQClient, error) {
	if w.client != nil {
		return w.client, nil
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(w.region))
	if err != nil {
		return nil, fmt.Errorf("aws config (region=%s): %w", w.region, err)
	}
	w.client = sqs.NewFromConfig(cfg)
	return w.client, nil
}

func (w *SESDLQDrainWorker) Start(ctx context.Context) {
	if w.db == nil {
		log.Printf("[SESDLQDrain] disabled (db missing)")
		return
	}
	go func() {
		log.Printf("SES DLQ drain worker started (interval=%s, queue=%s, webhook=%s)", w.interval, w.queueURL, w.webhookURL)
		w.tick(ctx)
		t := time.NewTicker(w.interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				w.tick(ctx)
			}
		}
	}()
}

func (w *SESDLQDrainWorker) tick(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	if sesDLQDisabled() {
		EmitHeartbeat(ctx, w.db, sesDLQWorkerName, int(w.interval.Seconds()), "disabled", "SES_DLQ_DRAIN_DISABLED set")
		return
	}
	lock := distlock.NewLock(w.redis, w.db, sesDLQLockKey, w.interval)
	acquired, err := lock.Acquire(ctx)
	if err != nil {
		log.Printf("[SESDLQDrain] lock acquire error: %v", err)
		return
	}
	if !acquired {
		return
	}
	defer func() {
		if err := lock.Release(context.Background()); err != nil {
			log.Printf("[SESDLQDrain] lock release error: %v", err)
		}
	}()
	st, err := w.RunOnce(ctx)
	if err != nil {
		log.Printf("[SESDLQDrain] %v", err)
		EmitHeartbeat(ctx, w.db, sesDLQWorkerName, int(w.interval.Seconds()), "error", err.Error())
		return
	}
	status, msg := "ok", fmt.Sprintf("replayed %d, failed %d, depth after %d", st.Replayed, st.Failed, st.DepthAfter)
	if st.DepthAfter > w.alertDepth || st.Failed > 0 {
		status = "error"
		msg = "SES DLQ not drained: " + msg
	}
	EmitHeartbeat(ctx, w.db, sesDLQWorkerName, int(w.interval.Seconds()), status, msg)
}

// SESDLQStats is one pass's result.
type SESDLQStats struct {
	Received   int
	Replayed   int
	Failed     int
	Deleted    int
	DepthAfter int
}

// RunOnce drains up to maxPerTick messages. Exported for tests; the caller
// owns locking.
func (w *SESDLQDrainWorker) RunOnce(ctx context.Context) (SESDLQStats, error) {
	var st SESDLQStats
	c, err := w.ensureClient(ctx)
	if err != nil {
		return st, err
	}
	for st.Received < w.maxPerTick && ctx.Err() == nil {
		out, err := c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:            aws.String(w.queueURL),
			MaxNumberOfMessages: 10,
			WaitTimeSeconds:     1,
			VisibilityTimeout:   sesDLQVisibilitySeconds,
		})
		if err != nil {
			return st, fmt.Errorf("receive: %w", err)
		}
		if len(out.Messages) == 0 {
			break
		}
		st.Received += len(out.Messages)
		var del []sqstypes.DeleteMessageBatchRequestEntry
		for _, m := range out.Messages {
			if m.Body == nil || m.ReceiptHandle == nil {
				st.Failed++
				continue
			}
			if w.replay(ctx, *m.Body) {
				st.Replayed++
				id := aws.ToString(m.MessageId)
				if len(id) > 80 {
					id = id[:80]
				}
				del = append(del, sqstypes.DeleteMessageBatchRequestEntry{Id: aws.String(id), ReceiptHandle: m.ReceiptHandle})
			} else {
				st.Failed++
			}
		}
		if len(del) > 0 {
			res, err := c.DeleteMessageBatch(ctx, &sqs.DeleteMessageBatchInput{QueueUrl: aws.String(w.queueURL), Entries: del})
			if err != nil {
				return st, fmt.Errorf("delete batch: %w", err)
			}
			st.Deleted += len(res.Successful)
		}
	}
	attrs, err := c.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: aws.String(w.queueURL),
		AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameApproximateNumberOfMessages}})
	if err == nil {
		st.DepthAfter, _ = strconv.Atoi(attrs.Attributes[string(sqstypes.QueueAttributeNameApproximateNumberOfMessages)])
	}
	if st.Received > 0 {
		log.Printf("[SESDLQDrain] received=%d replayed=%d failed=%d deleted=%d depth_after=%d", st.Received, st.Replayed, st.Failed, st.Deleted, st.DepthAfter)
	}
	return st, nil
}

// replay POSTs the SNS envelope to the local webhook exactly as SNS would.
func (w *SESDLQDrainWorker) replay(ctx context.Context, body string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.webhookURL, strings.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "text/plain; charset=UTF-8")
	req.Header.Set("x-amz-sns-message-type", "Notification")
	req.Header.Set("User-Agent", "Amazon Simple Notification Service Agent (replay: SESDLQDrainWorker)")
	resp, err := w.http.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}
