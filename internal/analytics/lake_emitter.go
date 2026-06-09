// Package analytics provides a best-effort, fire-and-forget emitter that fans
// out per-recipient email events to the Phase 1 S3 event lake via Amazon Data
// Firehose (stream "ignite-email-events" -> JSON->Parquet ->
// s3://ignite-analytics-lake/events/source=.../dt=...).
//
// Design contract (read before wiring new call sites):
//   - DISABLED BY DEFAULT. Init is a no-op unless an explicit stream name is
//     provided (wired from env ANALYTICS_FIREHOSE_STREAM in main.go). When
//     disabled, Emit is a cheap nil check — zero behavior change. This is what
//     makes it safe to ship dark and enable later via env without a code deploy.
//   - NEVER blocks the caller. Emit drops to a buffered channel; if the channel
//     is full the event is dropped and counted (analytics is lossy by design —
//     the authoritative store is the hot DB + the backfill). It must never add
//     latency or back-pressure to the send/ingest hot path.
//   - Idempotent at query time via event_uid; callers should pass a stable
//     event_uid (e.g. "ses:<eventID>", "pmta:<acctRowID>") so SNS/accounting
//     redeliveries collapse with SELECT DISTINCT event_uid in Athena.
package analytics

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"sync/atomic"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/firehose"
	fhtypes "github.com/aws/aws-sdk-go-v2/service/firehose/types"
)

// Event is one row of the ignite_analytics.email_events Glue table. Field names
// (json tags) MUST match the Glue column names exactly — the Firehose
// OpenXJsonSerDe maps by name. source/dt are partition keys extracted by the
// Firehose JQ processor; event_epoch_ms/ingested_at/dt are auto-filled.
type Event struct {
	EventUID          string `json:"event_uid"`
	RecipientSendID   string `json:"recipient_send_id"`
	CampaignID        string `json:"campaign_id"`
	SubscriberID      string `json:"subscriber_id"`
	Email             string `json:"email"`
	EmailDomain       string `json:"email_domain"`
	Brand             string `json:"brand"`
	ISPGroup          string `json:"isp_group"`
	RouteType         string `json:"route_type"`
	EventType         string `json:"event_type"`
	SuppressionReason string `json:"suppression_reason"`
	VMTA              string `json:"vmta"`
	Pool              string `json:"pool"`
	BounceCat         string `json:"bounce_cat"`
	DSNCode           string `json:"dsn_code"`
	DSNDiag           string `json:"dsn_diag"`
	LinkURL           string `json:"link_url"`
	SourceIP          string `json:"source_ip"`
	Variant           string `json:"variant"`
	EventAt           string `json:"event_at"`
	EventEpochMs      int64  `json:"event_epoch_ms"`
	IngestedAt        string `json:"ingested_at"`

	// partition keys (consumed by Firehose, also fine to keep in the record)
	Source string `json:"source"`
	Dt     string `json:"dt"`
}

const (
	maxBatch     = 500
	flushEvery   = 5 * time.Second
	channelDepth = 8192
)

// Emitter owns the Firehose client + background flush goroutine.
type Emitter struct {
	client  *firehose.Client
	stream  string
	ch      chan Event
	stop    chan struct{}
	done    chan struct{}
	dropped uint64
	sent    uint64
	failed  uint64
}

var (
	mu   sync.RWMutex
	lake *Emitter
)

// Init wires the global emitter. streamName == "" leaves analytics DISABLED
// (Emit becomes a no-op). Safe to call once at server start. region falls back
// to the default chain if empty.
func Init(ctx context.Context, streamName, region string) error {
	if streamName == "" {
		log.Printf("[analytics-lake] disabled (ANALYTICS_FIREHOSE_STREAM unset)")
		return nil
	}
	var opts []func(*awsconfig.LoadOptions) error
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return err
	}
	e := &Emitter{
		client: firehose.NewFromConfig(cfg),
		stream: streamName,
		ch:     make(chan Event, channelDepth),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	go e.run()
	mu.Lock()
	lake = e
	mu.Unlock()
	log.Printf("[analytics-lake] enabled stream=%s region=%s", streamName, region)
	return nil
}

// Emit queues an event for the lake. No-op when analytics is disabled. Never
// blocks: a full buffer drops the event (counted).
func Emit(ev Event) {
	mu.RLock()
	l := lake
	mu.RUnlock()
	if l == nil {
		return
	}
	l.enqueue(ev)
}

// Shutdown drains and flushes outstanding events. Best-effort; bounded wait.
func Shutdown() {
	mu.RLock()
	l := lake
	mu.RUnlock()
	if l == nil {
		return
	}
	close(l.stop)
	select {
	case <-l.done:
	case <-time.After(10 * time.Second):
	}
}

func (e *Emitter) enqueue(ev Event) {
	t := parseEventTime(ev.EventAt)
	ev.EventAt = t.UTC().Format("2006-01-02T15:04:05Z")
	ev.EventEpochMs = t.UTC().UnixMilli()
	ev.Dt = t.UTC().Format("2006-01-02")
	ev.IngestedAt = time.Now().UTC().Format("2006-01-02T15:04:05Z")
	if ev.Source == "" {
		ev.Source = "app"
	}
	select {
	case e.ch <- ev:
	default:
		atomic.AddUint64(&e.dropped, 1)
	}
}

func (e *Emitter) run() {
	defer close(e.done)
	ticker := time.NewTicker(flushEvery)
	defer ticker.Stop()
	buf := make([]Event, 0, maxBatch)

	flush := func() {
		if len(buf) == 0 {
			return
		}
		e.flush(buf)
		buf = buf[:0]
	}

	for {
		select {
		case <-e.stop:
			// drain whatever is queued, then flush and exit
			for {
				select {
				case ev := <-e.ch:
					buf = append(buf, ev)
					if len(buf) >= maxBatch {
						flush()
					}
				default:
					flush()
					return
				}
			}
		case ev := <-e.ch:
			buf = append(buf, ev)
			if len(buf) >= maxBatch {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (e *Emitter) flush(batch []Event) {
	recs := make([]fhtypes.Record, 0, len(batch))
	for i := range batch {
		b, err := json.Marshal(&batch[i])
		if err != nil {
			continue
		}
		b = append(b, '\n')
		recs = append(recs, fhtypes.Record{Data: b})
	}
	if len(recs) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := e.client.PutRecordBatch(ctx, &firehose.PutRecordBatchInput{
		DeliveryStreamName: &e.stream,
		Records:            recs,
	})
	if err != nil {
		atomic.AddUint64(&e.failed, uint64(len(recs)))
		log.Printf("[analytics-lake] put_record_batch error (%d dropped): %v", len(recs), err)
		return
	}
	failed := int32(0)
	if out.FailedPutCount != nil {
		failed = *out.FailedPutCount
	}
	if failed > 0 {
		// one retry for the failed subset
		var retry []fhtypes.Record
		for i, rr := range out.RequestResponses {
			if rr.ErrorCode != nil {
				retry = append(retry, recs[i])
			}
		}
		if len(retry) > 0 {
			if out2, err2 := e.client.PutRecordBatch(ctx, &firehose.PutRecordBatchInput{
				DeliveryStreamName: &e.stream, Records: retry,
			}); err2 == nil && out2.FailedPutCount != nil {
				failed = *out2.FailedPutCount
			}
		}
	}
	atomic.AddUint64(&e.sent, uint64(len(recs)-int(failed)))
	if failed > 0 {
		atomic.AddUint64(&e.failed, uint64(failed))
	}
}

// Stats returns counters for health endpoints. Returns zeros when disabled.
func Stats() (sent, failed, dropped uint64, enabled bool) {
	mu.RLock()
	l := lake
	mu.RUnlock()
	if l == nil {
		return 0, 0, 0, false
	}
	return atomic.LoadUint64(&l.sent), atomic.LoadUint64(&l.failed), atomic.LoadUint64(&l.dropped), true
}

func parseEventTime(s string) time.Time {
	if s == "" {
		return time.Now().UTC()
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05Z", "2006-01-02T15:04:05.000Z"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Now().UTC()
}

// CanonicalEventType maps internal event-type spellings to the lake's canonical
// vocabulary so PMTA, SES, and app sources line up in one fact table.
func CanonicalEventType(in string) string {
	switch in {
	case "opened":
		return "open"
	case "clicked":
		return "click"
	case "sent":
		return "attempted"
	case "deferred":
		return "delivery_delay"
	case "complained":
		return "complaint"
	default:
		return in
	}
}
