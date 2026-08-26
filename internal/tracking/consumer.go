package tracking

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"

	"github.com/ignite/sparkpost-monitor/internal/mailing"
	"github.com/ignite/sparkpost-monitor/internal/pkg/brand"
)

// BrandSuppressor writes a BRAND-SCOPED suppression into the store the send
// path enforces for brand scoping (mailing_domain_suppressions + the hub's
// in-memory brandSet, checked by IsSuppressedForBrand at plan and send time).
// Satisfied by *engine.GlobalSuppressionHub.
//
// Doctrine (operator 2026-07-13, CLAUDE.md §12 amendment set): brands are
// SEPARATE senders. An unsubscribe from one brand suppresses the email for
// THAT brand only — it must never write mailing_global_suppressions, which
// would suppress across all 16 brands.
type BrandSuppressor interface {
	SuppressScoped(ctx context.Context, email, brandRoot, reason, source, isp, sourceIP, campaign string) error
}

type Consumer struct {
	sqsClient *sqs.Client
	queueURL  string
	db        *sql.DB
	// brandSuppressor is wired from main.go. Guarded by wiringMu because the
	// hub is constructed inside SetMailingDB, which main.go runs in a
	// GOROUTINE (route registration is heavy) — so on every boot the hub is
	// still nil when this consumer starts, and the wiring must be applied
	// LATE, after Start, from that goroutine. A plain field here was the
	// 2026-08-21 defect: the boot-time type assertion saw nil, logged
	// "hub NOT wired", and every SQS unsubscribe skipped the enforced set
	// for the life of the process.
	wiringMu        sync.RWMutex
	brandSuppressor BrandSuppressor
	done            chan struct{}
}

func NewConsumer(sqsClient *sqs.Client, queueURL string, db *sql.DB) *Consumer {
	return &Consumer{
		sqsClient: sqsClient,
		queueURL:  queueURL,
		db:        db,
		done:      make(chan struct{}),
	}
}

// SetBrandSuppressor wires the suppression hub's brand-scoped write path so
// unsubscribes processed off SQS reach the email-level enforcement set the
// send path checks (IsSuppressedForBrand) — not only the legacy
// mailing_suppressions table. Without this, a t.em-routed unsubscribe flips
// ONE subscriber row and sibling rows for the same email IN THE SAME BRAND
// stay mailable (CAN-SPAM exposure).
// Safe to call before OR after Start — late wiring is the NORMAL path (see
// the wiringMu comment above).
func (c *Consumer) SetBrandSuppressor(s BrandSuppressor) {
	c.wiringMu.Lock()
	c.brandSuppressor = s
	c.wiringMu.Unlock()
}

func (c *Consumer) getBrandSuppressor() BrandSuppressor {
	c.wiringMu.RLock()
	defer c.wiringMu.RUnlock()
	return c.brandSuppressor
}

// pollWorkers controls how many concurrent SQS receive loops run per task.
// Each worker long-polls independently; within a batch messages are fanned
// out via goroutines. With 2 ECS tasks × 16 workers × 10-msg batches plus
// the idempotency fast-path, the consumer can drain large redelivery
// backlogs at >2k msg/min while each new event still flows through the
// full insert + aggregate update + engagement recompute pipeline.
const pollWorkers = 16

func (c *Consumer) Start(ctx context.Context) {
	log.Printf("SQS tracking consumer started (queue=%s, workers=%d)", c.queueURL, pollWorkers)
	for i := 0; i < pollWorkers; i++ {
		go c.poll(ctx)
	}
}

func (c *Consumer) Stop() {
	close(c.done)
}

func (c *Consumer) poll(ctx context.Context) {
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
			VisibilityTimeout:   120,
		})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("SQS receive error: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}

		if len(out.Messages) == 0 {
			continue
		}

		var wg sync.WaitGroup
		for _, msg := range out.Messages {
			wg.Add(1)
			go func(msg sqstypes.Message) {
				defer wg.Done()

				var evt TrackingEvent
				if err := json.Unmarshal([]byte(*msg.Body), &evt); err != nil {
					log.Printf("SQS bad message: %v", err)
					c.deleteMessage(ctx, msg.ReceiptHandle)
					return
				}

				if err := c.processEvent(ctx, evt); err != nil {
					log.Printf("SQS process error (%s): %v", evt.EventType, err)
					return
				}

				c.deleteMessage(ctx, msg.ReceiptHandle)
			}(msg)
		}
		wg.Wait()
	}
}

func (c *Consumer) deleteMessage(ctx context.Context, handle *string) {
	c.sqsClient.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(c.queueURL),
		ReceiptHandle: handle,
	})
}

func (c *Consumer) processEvent(ctx context.Context, evt TrackingEvent) error {
	switch evt.EventType {
	case EventOpen:
		return c.processOpen(ctx, evt)
	case EventClick:
		return c.processClick(ctx, evt)
	case EventUnsubscribe:
		return c.processUnsubscribe(ctx, evt)
	default:
		log.Printf("unknown event type: %s", evt.EventType)
		return nil
	}
}

func (c *Consumer) processOpen(ctx context.Context, evt TrackingEvent) error {
	orgID, _ := uuid.Parse(evt.OrgID)
	campaignID, _ := uuid.Parse(evt.CampaignID)
	subscriberID, _ := uuid.Parse(evt.SubscriberID)
	emailID, _ := uuid.Parse(evt.EmailID)

	// Fast-path idempotency check: if this exact (id, event_at) pair is already
	// in the tracking_events partition, skip all downstream work. This lets us
	// drain SQS redelivery backlogs at 10-20x the rate since no aggregate
	// UPDATEs / engagement recomputes run for duplicates.
	var exists bool
	if err := c.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM mailing_tracking_events
			WHERE id = $1 AND event_at = $2 AND event_type = 'opened'
		)
	`, emailID, evt.Timestamp).Scan(&exists); err == nil && exists {
		return nil
	}

	// Cross-source dedupe: the (id, event_at) PK on mailing_tracking_events
	// only catches exact-timestamp duplicate webhook deliveries. A pixel
	// fire (via api.HandleTrackOpen) followed by a provider webhook for
	// the same open would slip past the fast-path above. mailing_open_dedupe
	// is keyed on (campaign_id, subscriber_id) and is the single source of
	// truth for "have we counted this subscriber's open of this campaign
	// at least once". If the row already exists we assume the API path
	// already moved every counter and we should not move them again here.
	var dedupeOK bool
	derr := c.db.QueryRowContext(ctx, `
		INSERT INTO mailing_open_dedupe (campaign_id, subscriber_id, email_id, first_seen_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (campaign_id, subscriber_id) DO NOTHING
		RETURNING true
	`, campaignID, subscriberID, emailID, evt.Timestamp).Scan(&dedupeOK)
	switch {
	case errors.Is(derr, sql.ErrNoRows):
		return nil
	case derr != nil:
		// Fail closed on dedupe gate errors — same posture as the API handler.
		log.Printf("processOpen dedupe error: %v — skipping side effects camp=%s sub=%s",
			derr, campaignID, subscriberID)
		return nil
	case !dedupeOK:
		return nil
	}

	var email string
	c.db.QueryRowContext(ctx, `SELECT email FROM mailing_subscribers WHERE id = $1`, subscriberID).Scan(&email)

	// MPP detection: open within 120s of the sent event (sent timestamp is reliable; delivered is batch-delayed)
	isMachineOpen := false
	var refAt time.Time
	if err := c.db.QueryRowContext(ctx, `
		SELECT event_at FROM mailing_tracking_events
		WHERE subscriber_id = $1 AND campaign_id = $2 AND event_type = 'sent'
		ORDER BY event_at DESC LIMIT 1
	`, subscriberID, campaignID).Scan(&refAt); err == nil {
		if evt.Timestamp.Sub(refAt) <= 120*time.Second {
			isMachineOpen = true
		}
	}

	// Self-contained event row: recipient_domain is populated at insert
	// time via LEFT JOIN to mailing_subscribers so per-ISP analytics
	// never has to reconstruct the bucket via a join-back at report
	// time. If the subscriber row is missing (deleted / migrated),
	// recipient_domain stays NULL on this row and the report layer
	// surfaces it as `unresolved_subscriber` instead of silently folding
	// into `other`. We deliberately do NOT also write a denormalized
	// `email` column: the schema does not have one, and subscriber_id
	// is already the canonical FK to recover the email when needed.
	res, err := c.db.ExecContext(ctx, `
		INSERT INTO mailing_tracking_events (id, organization_id, campaign_id, subscriber_id, event_type, event_at, ip_address, user_agent, device_type, sending_domain, recipient_domain, is_machine_open)
		SELECT $1, $2, $3, $4, 'opened', $5, $6, $7, $8,
			LOWER(SPLIT_PART(c.from_email, '@', 2)),
			LOWER(SPLIT_PART(s.email, '@', 2)),
			$9
		FROM mailing_campaigns c
		LEFT JOIN mailing_subscribers s ON s.id = $4::uuid
		WHERE c.id = $3
		ON CONFLICT DO NOTHING
	`, emailID, orgID, campaignID, subscriberID, evt.Timestamp, evt.IPAddress, evt.UserAgent, detectDevice(evt.UserAgent), isMachineOpen)
	if err != nil {
		return err
	}

	// Guard the aggregate UPDATEs: only run them if the INSERT actually wrote
	// a row. Without this, duplicate SQS deliveries double-count open_count etc.
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}

	if !isMachineOpen {
		c.db.ExecContext(ctx, `UPDATE mailing_campaigns SET open_count = open_count + 1 WHERE id = $1`, campaignID)
		c.db.ExecContext(ctx, `UPDATE mailing_subscribers SET total_opens = total_opens + 1, last_open_at = NOW(), updated_at = NOW() WHERE id = $1`, subscriberID)
		// Inbox-profile counter goes through the email_hash UNIQUE key, matching
		// every other writer of this table (pixel handler, send_worker, ingest,
		// campaign_builder_send_sync, mailing_lists_handlers). The previous
		// `WHERE email = $1` form was AWS RDS Performance Insights' top SQL —
		// non-unique TEXT index, mixed casing, fan-out on empty-string lookups.
		// See plan: .cursor/plans/inbox_profile_update_hot_path_*.plan.md
		if email != "" {
			emailLower := strings.ToLower(strings.TrimSpace(email))
			eHash := hashEmail(emailLower)
			domain := domainFromEmail(emailLower)
			c.db.ExecContext(ctx, `
				INSERT INTO mailing_inbox_profiles (id, email_hash, email, domain, total_opens, last_open_at, updated_at)
				VALUES (gen_random_uuid(), $1, $2, $3, 1, NOW(), NOW())
				ON CONFLICT (email_hash) DO UPDATE SET
					total_opens = mailing_inbox_profiles.total_opens + 1,
					last_open_at = NOW(),
					updated_at = NOW()
			`, eHash, emailLower, domain)
		} else {
			log.Printf("PROCESSED OPEN: empty subscriber email, skipping inbox_profile upsert campaign=%s subscriber=%s", campaignID, subscriberID)
		}
		c.updateEngagementScore(ctx, subscriberID)
		log.Printf("PROCESSED OPEN: campaign=%s subscriber=%s email=%s mpp=false", campaignID, subscriberID, email)
	} else {
		log.Printf("PROCESSED OPEN (MPP skipped aggregates): campaign=%s subscriber=%s email=%s", campaignID, subscriberID, email)
	}

	// Per-domain engagement state (SA-1). Runs for BOTH MPP and human opens:
	// an MPP open is strict inbox-placement proof and the per-domain state
	// machine treats any open as engagement signal (operator directive
	// 2026-05-09). Without this, the SQS consumer path would never promote
	// any subscriber from probe → engaged, and the cross-engaged graduation
	// pass would stay empty forever — observed in production with 0 engaged
	// rows across 625k probe rows after the SA-1 deploy.
	sdsDomain := mailing.ResolveSendingDomainForCampaign(ctx, c.db, campaignID)
	mailing.UpsertSDSOpen(ctx, c.db, subscriberID, sdsDomain)
	mailing.RecomputeSDSScoreLocal(ctx, c.db, subscriberID, sdsDomain)
	return nil
}

// clickNamespace derives a deterministic UUID per (campaign, subscriber,
// timestamp, link_url) tuple so duplicate SQS deliveries hit ON CONFLICT
// instead of creating duplicate click rows.
var clickNamespace = uuid.MustParse("2f9c9d2c-2f3e-4f5a-9b1a-0b8c7e2a0001")

func clickDeterministicID(campaignID, subscriberID uuid.UUID, ts time.Time, linkURL string) uuid.UUID {
	key := campaignID.String() + "|" + subscriberID.String() + "|" + ts.UTC().Format(time.RFC3339Nano) + "|" + linkURL
	return uuid.NewSHA1(clickNamespace, []byte(key))
}

func (c *Consumer) processClick(ctx context.Context, evt TrackingEvent) error {
	orgID, _ := uuid.Parse(evt.OrgID)
	campaignID, _ := uuid.Parse(evt.CampaignID)
	subscriberID, _ := uuid.Parse(evt.SubscriberID)
	clickID := clickDeterministicID(campaignID, subscriberID, evt.Timestamp, evt.LinkURL)

	// Fast-path idempotency check — same reasoning as processOpen.
	var exists bool
	if err := c.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM mailing_tracking_events
			WHERE id = $1 AND event_at = $2 AND event_type = 'clicked'
		)
	`, clickID, evt.Timestamp).Scan(&exists); err == nil && exists {
		return nil
	}

	var email string
	c.db.QueryRowContext(ctx, `SELECT email FROM mailing_subscribers WHERE id = $1`, subscriberID).Scan(&email)

	// SA-5: classify the click as machine vs human BEFORE the INSERT so
	// the column is populated atomically with the row. INFORMATIONAL
	// only in this release — segments and engagement state still treat
	// every click as engagement signal regardless of the verdict.
	//
	// Trade-off: the SQS payload does not carry the original send
	// timestamp, and looking it up in mailing_tracking_events would add
	// a second blocking round-trip per message in the consumer's hot
	// fan-out path (16 workers × 10-msg batches). We pass 0 for the
	// delta which collapses the classifier to rules 1 and 2 only
	// (explicit scanner UA + bare-Mozilla-on-cloud-IP). The HTTP pixel
	// path in api.HandleTrackClick performs the lookup since it runs
	// per-request and the cost is amortized across the redirect.
	isMachineClick := ClassifyClickAsMachine(evt.UserAgent, evt.IPAddress, 0)

	// Self-contained event row — see processOpen for the same rationale.
	res, err := c.db.ExecContext(ctx, `
		INSERT INTO mailing_tracking_events (id, organization_id, campaign_id, subscriber_id, event_type, event_at, ip_address, user_agent, device_type, link_url, sending_domain, recipient_domain, is_machine_click)
		SELECT $1, $2, $3, $4, 'clicked', $5, $6, $7, $8, $9,
			LOWER(SPLIT_PART(c.from_email, '@', 2)),
			LOWER(SPLIT_PART(s.email, '@', 2)),
			$10
		FROM mailing_campaigns c
		LEFT JOIN mailing_subscribers s ON s.id = $4::uuid
		WHERE c.id = $3
		ON CONFLICT DO NOTHING
	`, clickID, orgID, campaignID, subscriberID, evt.Timestamp, evt.IPAddress, evt.UserAgent, detectDevice(evt.UserAgent), evt.LinkURL, isMachineClick)
	if err != nil {
		return err
	}

	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}

	c.db.ExecContext(ctx, `UPDATE mailing_campaigns SET click_count = click_count + 1 WHERE id = $1`, campaignID)
	c.db.ExecContext(ctx, `UPDATE mailing_subscribers SET total_clicks = total_clicks + 1, last_click_at = NOW(), updated_at = NOW() WHERE id = $1`, subscriberID)
	// Inbox-profile counter via email_hash UNIQUE key — matches every other
	// writer of this table. See processOpen for the full rationale.
	if email != "" {
		emailLower := strings.ToLower(strings.TrimSpace(email))
		eHash := hashEmail(emailLower)
		domain := domainFromEmail(emailLower)
		c.db.ExecContext(ctx, `
			INSERT INTO mailing_inbox_profiles (id, email_hash, email, domain, total_clicks, last_click_at, updated_at)
			VALUES (gen_random_uuid(), $1, $2, $3, 1, NOW(), NOW())
			ON CONFLICT (email_hash) DO UPDATE SET
				total_clicks = mailing_inbox_profiles.total_clicks + 1,
				last_click_at = NOW(),
				updated_at = NOW()
		`, eHash, emailLower, domain)
	} else {
		log.Printf("PROCESSED CLICK: empty subscriber email, skipping inbox_profile upsert campaign=%s subscriber=%s", campaignID, subscriberID)
	}
	c.updateEngagementScore(ctx, subscriberID)

	// Per-domain engagement state (SA-1). Every click is treated as engagement
	// signal regardless of the is_machine_click verdict — the verdict is
	// informational in this release. Mirrors the SDS write at the end of
	// processOpen so SQS consumer events feed the state machine.
	sdsDomain := mailing.ResolveSendingDomainForCampaign(ctx, c.db, campaignID)
	mailing.UpsertSDSClick(ctx, c.db, subscriberID, sdsDomain)
	mailing.RecomputeSDSScoreLocal(ctx, c.db, subscriberID, sdsDomain)

	log.Printf("PROCESSED CLICK: campaign=%s subscriber=%s url=%s", campaignID, subscriberID, evt.LinkURL)
	return nil
}

func (c *Consumer) processUnsubscribe(ctx context.Context, evt TrackingEvent) error {
	orgID, _ := uuid.Parse(evt.OrgID)
	campaignID, _ := uuid.Parse(evt.CampaignID)
	subscriberID, _ := uuid.Parse(evt.SubscriberID)

	var email string
	c.db.QueryRowContext(ctx, `SELECT email FROM mailing_subscribers WHERE id = $1`, subscriberID).Scan(&email)

	// Brand-scoped suppression FIRST, before any other write. The hub's
	// brand axis (mailing_domain_suppressions, checked by
	// IsSuppressedForBrand at pmta_campaign_planner.go qualifyEmail and
	// send_worker.go claim time) is the only email-level guard the send
	// path enforces — the row-keyed writes below cover ONE subscriber row,
	// and the legacy mailing_suppressions write is not read by that path.
	//
	// Scope doctrine (operator 2026-07-13): the unsubscribe is honored for
	// the ORIGINATING brand only — sibling brands keep mailing this email.
	// NEVER write mailing_global_suppressions here. SuppressScoped is
	// idempotent (ON CONFLICT (organization_id, email_hash, brand_root))
	// and carries the hub's own org id, so a malformed evt.OrgID cannot
	// misroute the row. Ordering it first means a failure returns before
	// any side effects, so the SQS redelivery retry re-runs cleanly.
	sdsDomain := mailing.ResolveSendingDomainForCampaign(ctx, c.db, campaignID)
	if email != "" {
		brandRoot := brand.Root(sdsDomain)
		suppressor := c.getBrandSuppressor()
		switch {
		case suppressor == nil:
			// Start now happens after wiring (main.go late pass), so this is
			// a true wiring regression. Returning the error keeps the SQS
			// message alive — redelivery retries until the hub attaches —
			// instead of deleting it with enforcement silently skipped
			// (the pre-2026-08-26 boot-window defect).
			log.Printf("ERROR: PROCESS UNSUB brand suppression hub NOT wired — returning message to SQS for redelivery: campaign=%s subscriber=%s md5=%s",
				campaignID, subscriberID, hashEmail(strings.ToLower(strings.TrimSpace(email))))
			return fmt.Errorf("brand suppression hub not wired; unsubscribe for campaign %s not enforceable yet", campaignID)
		case brandRoot == "":
			// Campaign lookup failed or campaign has no from_email — the
			// brand cannot be resolved, so the email-level suppression
			// cannot be scoped. Retrying would not fix this, so log loudly
			// and fall through to the single-row legacy writes.
			log.Printf("ERROR: PROCESS UNSUB brand root unresolved (campaign=%s) — unsubscribe enforced on subscriber row only, NOT at email level: subscriber=%s md5=%s",
				campaignID, subscriberID, hashEmail(strings.ToLower(strings.TrimSpace(email))))
		default:
			if err := suppressor.SuppressScoped(ctx, email, brandRoot, "user_unsubscribe", "sqs_tracking", "", evt.IPAddress, evt.CampaignID); err != nil {
				log.Printf("ERROR: PROCESS UNSUB brand suppression write failed (message will be retried): campaign=%s subscriber=%s brand=%s md5=%s err=%v",
					campaignID, subscriberID, brandRoot, hashEmail(strings.ToLower(strings.TrimSpace(email))), err)
				return err // do NOT delete the SQS message — redelivery retries the suppression
			}
		}
	}

	c.db.ExecContext(ctx, `
		INSERT INTO mailing_tracking_events (id, organization_id, campaign_id, subscriber_id, event_type, event_at, ip_address, user_agent)
		VALUES ($1, $2, $3, $4, 'unsubscribed', $5, $6, $7)
	`, uuid.New(), orgID, campaignID, subscriberID, evt.Timestamp, evt.IPAddress, evt.UserAgent)

	c.db.ExecContext(ctx, `UPDATE mailing_subscribers SET status = 'unsubscribed', updated_at = NOW() WHERE id = $1`, subscriberID)
	c.db.ExecContext(ctx, `UPDATE mailing_campaigns SET unsubscribe_count = COALESCE(unsubscribe_count, 0) + 1 WHERE id = $1`, campaignID)
	c.db.ExecContext(ctx, `
		INSERT INTO mailing_suppressions (id, email, reason, source, active, created_at, updated_at)
		VALUES ($1, $2, 'User unsubscribed', 'unsubscribe', true, NOW(), NOW())
		ON CONFLICT (email) DO UPDATE SET active = true, reason = 'User unsubscribed', updated_at = NOW()
	`, uuid.New(), email)

	// SDS state-machine: an unsubscribe is a hard suppression signal for this
	// sending domain. UpsertSDSUnsub sets state='suppressed' so the audience
	// finalizer's SDS exclusion clause prunes them on the next campaign.
	// (sdsDomain resolved above, before the brand-scoped suppression write.)
	mailing.UpsertSDSUnsub(ctx, c.db, subscriberID, sdsDomain)

	log.Printf("PROCESSED UNSUB: campaign=%s subscriber=%s email=%s", campaignID, subscriberID, email)
	return nil
}

func (c *Consumer) updateEngagementScore(ctx context.Context, subscriberID uuid.UUID) {
	var totalOpens, totalClicks, totalEmails int
	var lastOpenAt *time.Time
	c.db.QueryRowContext(ctx, `
		SELECT COALESCE(total_opens,0), COALESCE(total_clicks,0), COALESCE(total_emails_received,1), last_open_at
		FROM mailing_subscribers WHERE id = $1
	`, subscriberID).Scan(&totalOpens, &totalClicks, &totalEmails, &lastOpenAt)

	if totalEmails < 1 {
		totalEmails = 1
	}
	openRate := float64(totalOpens) / float64(totalEmails) * 100
	clickRate := float64(totalClicks) / float64(totalEmails) * 100
	score := (openRate * 0.4) + (clickRate * 0.6)

	if lastOpenAt != nil && time.Since(*lastOpenAt) < 7*24*time.Hour {
		score += 20
	} else if lastOpenAt != nil && time.Since(*lastOpenAt) < 30*24*time.Hour {
		score += 10
	}
	if score > 100 {
		score = 100
	}

	c.db.ExecContext(ctx, `UPDATE mailing_subscribers SET engagement_score = $2, updated_at = NOW() WHERE id = $1`, subscriberID, score)
}

func detectDevice(ua string) string {
	ua = strings.ToLower(ua)
	if strings.Contains(ua, "mobile") || strings.Contains(ua, "android") || strings.Contains(ua, "iphone") {
		return "mobile"
	}
	if strings.Contains(ua, "tablet") || strings.Contains(ua, "ipad") {
		return "tablet"
	}
	return "desktop"
}
