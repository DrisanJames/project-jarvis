package engine

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/analytics"
	"github.com/ignite/sparkpost-monitor/internal/pkg/smtputil"
	"github.com/redis/go-redis/v9"
)

func ingestEmailHash(email string) string {
	h := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
	return fmt.Sprintf("%x", h)
}

// campaignRecorder abstracts campaign event recording for testability.
type campaignRecorder interface {
	RecordEvent(e CampaignEvent)
}

// bounceWindow tracks sent and hard-bounced counts per campaign in a
// tumbling 5-minute window for real-time bounce rate alerting.
type bounceWindow struct {
	mu       sync.Mutex
	sent     map[string]int // campaignID → count
	bounced  map[string]int // campaignID → hard bounce count
	names    map[string]string // campaignID → campaign name
	windowStart time.Time
}

func newBounceWindow() *bounceWindow {
	return &bounceWindow{
		sent:        make(map[string]int),
		bounced:     make(map[string]int),
		names:       make(map[string]string),
		windowStart: time.Now(),
	}
}

const bounceWindowDuration = 5 * time.Minute
const hardBounceRateThreshold = 0.02   // 2%
const softBounceRateThreshold = 0.10   // 10%

// VersionEngineAccountingWebhook is the /engine/webhook response contract; bump on behavior change.
const VersionEngineAccountingWebhook = "2.0"

const (
	defaultAccountingWebhookWorkers = 12
	defaultAccountingWebhookQueue   = 4096
)

func (bw *bounceWindow) record(campaignID, campaignName, eventType string, isHardBounce bool) (rate float64, exceeded bool) {
	bw.mu.Lock()
	defer bw.mu.Unlock()

	if time.Since(bw.windowStart) > bounceWindowDuration {
		bw.sent = make(map[string]int)
		bw.bounced = make(map[string]int)
		bw.names = make(map[string]string)
		bw.windowStart = time.Now()
	}

	bw.names[campaignID] = campaignName

	switch eventType {
	case "delivered", "bounced":
		bw.sent[campaignID]++
	}

	if eventType == "bounced" && isHardBounce {
		bw.bounced[campaignID]++
	}

	sent := bw.sent[campaignID]
	if sent < 50 {
		return 0, false
	}
	rate = float64(bw.bounced[campaignID]) / float64(sent)
	return rate, rate > hardBounceRateThreshold
}

// Ingestor receives PMTA accounting records via webhook and polls PMTA
// status APIs. It classifies each record by ISP and fans out to the
// SignalProcessor, agent clusters, CampaignEventTracker, and
// GlobalSuppressionHub.
type Ingestor struct {
	registry  *ISPRegistry
	processor *SignalProcessor
	tracker   campaignRecorder
	globalHub *GlobalSuppressionHub
	alerter   *Alerter
	db        *sql.DB

	// Record listeners (agents subscribe to their ISP's records)
	listeners map[ISP][]chan<- AccountingRecord

	// Real-time bounce rate monitoring
	bounceWin *bounceWindow

	// PMTA management API polling
	pmtaHost     string
	pmtaPort     int
	pmtaUser     string
	pmtaPassword string
	pollInterval time.Duration
	httpClient   *http.Client

	redisClient *redis.Client

	// Async accounting webhook (PMTA drainer POSTs). Nil disables queue — HandleWebhook runs synchronously.
	webhookJobs chan []AccountingRecord
}

// IngestorConfig holds configuration for the ingestor.
type IngestorConfig struct {
	PMTAHost     string
	PMTAPort     int
	PMTAUser     string
	PMTAPassword string
	PollInterval time.Duration
}

// SetAlerter attaches the alerter for infrastructure health notifications.
func (ing *Ingestor) SetAlerter(a *Alerter) {
	ing.alerter = a
}

// SetCampaignTracker attaches the campaign event tracker to the ingestor.
func (ing *Ingestor) SetCampaignTracker(t *CampaignEventTracker) {
	ing.tracker = t
}

// SetGlobalSuppressionHub attaches the global suppression hub so that
// every negative signal (bounce, complaint) is auto-suppressed globally.
func (ing *Ingestor) SetGlobalSuppressionHub(hub *GlobalSuppressionHub) {
	ing.globalHub = hub
}

// SetDB attaches a database handle for persisting accounting events to
// mailing_tracking_events and updating mailing_campaigns counters.
func (ing *Ingestor) SetDB(db *sql.DB) {
	ing.db = db
}

// NewIngestor creates a new data ingestor.
func NewIngestor(registry *ISPRegistry, processor *SignalProcessor, cfg IngestorConfig) *Ingestor {
	interval := cfg.PollInterval
	if interval == 0 {
		interval = 10 * time.Second
	}
	return &Ingestor{
		registry:     registry,
		processor:    processor,
		listeners:    make(map[ISP][]chan<- AccountingRecord),
		bounceWin:    newBounceWindow(),
		pmtaHost:     cfg.PMTAHost,
		pmtaPort:     cfg.PMTAPort,
		pmtaUser:     cfg.PMTAUser,
		pmtaPassword: cfg.PMTAPassword,
		pollInterval: interval,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		},
	}
}

// SetRedisClient sets the Redis client for engagement telemetry writes.
func (ing *Ingestor) SetRedisClient(c *redis.Client) {
	ing.redisClient = c
}

// SubscribeISP registers a listener for records classified to a specific ISP.
func (ing *Ingestor) SubscribeISP(isp ISP, ch chan<- AccountingRecord) {
	ing.listeners[isp] = append(ing.listeners[isp], ch)
}

// StartAccountingWebhookWorkers starts goroutines that process PMTA JSON batches
// from the drainer. Idempotent with multiple calls.
func (ing *Ingestor) StartAccountingWebhookWorkers(ctx context.Context) {
	if ing.webhookJobs != nil {
		return
	}
	q := defaultAccountingWebhookQueue
	if v := os.Getenv("ENGINE_ACCOUNTING_WEBHOOK_QUEUE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			q = n
		}
	}
	w := defaultAccountingWebhookWorkers
	if v := os.Getenv("ENGINE_ACCOUNTING_WEBHOOK_WORKERS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			w = n
		}
	}
	ing.webhookJobs = make(chan []AccountingRecord, q)
	for i := 0; i < w; i++ {
		go ing.accountingWebhookWorker(ctx)
	}
	log.Printf("[ingest] accounting webhook async: workers=%d queue=%d version=%s", w, q, VersionEngineAccountingWebhook)
}

func (ing *Ingestor) accountingWebhookWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case batch := <-ing.webhookJobs:
			if len(batch) == 0 {
				continue
			}
			ing.processAccountingWebhookBatch(batch)
		}
	}
}

// processAccountingWebhookBatch runs the full per-record accounting pipeline (formerly inline in HandleWebhook).
// Returns the same processed count semantics as the legacy handler (one count per record in batch).
func (ing *Ingestor) processAccountingWebhookBatch(records []AccountingRecord) int {
	processed := 0
	defaultPoolCount := 0
	for _, rec := range records {
		isp := ing.classifyRecord(rec)

		if rec.VMTA == "{default}" || rec.Pool == "{default}" {
			defaultPoolCount++
			log.Printf("[ingest-ALERT] default-pool leak: recipient=%s vmta=%s pool=%s type=%s",
				rec.Recipient, rec.VMTA, rec.Pool, rec.Type)
		}

		if ing.globalHub != nil {
			ing.routeToGlobalSuppression(rec, isp)
		}
		if ing.db != nil {
			ing.persistToDB(rec, isp)
		}

		if isp == "" {
			processed++
			continue
		}

		ing.processor.Ingest(isp, rec)

		for _, ch := range ing.listeners[isp] {
			select {
			case ch <- rec:
			default:
			}
		}

		if ing.tracker != nil && rec.JobID != "" {
			ing.routeToCampaignTracker(rec, isp)
		}

		processed++
	}

	if defaultPoolCount > 0 && ing.alerter != nil {
		ing.alerter.SendDefaultPoolAlert(defaultPoolCount, "{default}")
	}
	return processed
}

// HandleWebhook is the HTTP handler for PMTA accounting webhook POSTs.
// Expects JSON array of AccountingRecord objects.
func (ing *Ingestor) HandleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var records []AccountingRecord
	if err := json.Unmarshal(body, &records); err != nil {
		var single AccountingRecord
		if err2 := json.Unmarshal(body, &single); err2 == nil {
			records = []AccountingRecord{single}
		} else {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
	}

	if len(records) == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Engine-Accounting-Version", VersionEngineAccountingWebhook)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"received":    0,
			"queued":      0,
			"api_version": VersionEngineAccountingWebhook,
		})
		return
	}

	sync := os.Getenv("ENGINE_ACCOUNTING_WEBHOOK_SYNC") == "1" || ing.webhookJobs == nil
	if sync {
		n := ing.processAccountingWebhookBatch(records)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Engine-Accounting-Version", VersionEngineAccountingWebhook)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"received":     len(records),
			"processed":    n,
			"api_version":  VersionEngineAccountingWebhook,
			"synchronous": true,
		})
		return
	}

	select {
	case ing.webhookJobs <- records:
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Engine-Accounting-Version", VersionEngineAccountingWebhook)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"received":    len(records),
			"queued":      len(records),
			"api_version": VersionEngineAccountingWebhook,
		})
	default:
		http.Error(w, "accounting queue saturated", http.StatusServiceUnavailable)
	}
}

func (ing *Ingestor) routeToCampaignTracker(rec AccountingRecord, isp ISP) {
	var eventType string
	switch rec.Type {
	case "d":
		// SES relay handoff is not a recipient delivery — keep it out of the
		// live "delivered" tracker so dashboards match SES truth.
		if IsPMTARelayedToSES(rec) {
			eventType = "relayed_to_ses"
		} else {
			eventType = "delivered"
		}
	case "b":
		eventType = ClassifyBounce(rec.BounceCat)
	case "t", "tq":
		eventType = "deferred"
	case "f":
		eventType = "complaint"
	default:
		return
	}

	ing.tracker.RecordEvent(CampaignEvent{
		CampaignID: rec.JobID,
		EventType:  eventType,
		Recipient:  rec.Recipient,
		ISP:        string(isp),
		SourceIP:   rec.SourceIP,
		BounceType: rec.BounceCat,
		DSNCode:    rec.DSNStatus,
		DSNDiag:    rec.DSNDiag,
		Timestamp:  time.Now(),
	})

	// Phase 1 event lake (best-effort, no-op unless enabled). Mirrors the PMTA
	// side of the backfill so the live feed continues seamlessly. event_uid is
	// derived from PMTA's stable per-record fields so HTTP-bridge redeliveries
	// dedupe in Athena via SELECT DISTINCT event_uid.
	src := rec.Source
	if src == "" {
		src = "pmta"
	}
	routeType := src + "_direct"
	if IsPMTARelayedToSES(rec) {
		routeType = "ses_tenant"
	}
	analytics.Emit(analytics.Event{
		EventUID:    src + ":" + rec.JobID + ":" + rec.Recipient + ":" + rec.Type + ":" + rec.DeliveryTime,
		CampaignID:  rec.JobID,
		Email:       rec.Recipient,
		EmailDomain: rec.Domain,
		ISPGroup:    string(isp),
		RouteType:   routeType,
		EventType:   analytics.CanonicalEventType(eventType),
		VMTA:        rec.VMTA,
		Pool:        rec.Pool,
		BounceCat:   rec.BounceCat,
		DSNCode:     rec.DSNStatus,
		DSNDiag:     rec.DSNDiag,
		SourceIP:    rec.SourceIP,
		EventAt:     time.Now().UTC().Format(time.RFC3339),
		Source:      src,
	})
}

// routeToGlobalSuppression suppresses permanent failures and recipient-quality
// signals: hard bounces, quota-issues (Yahoo treats full-mailbox as a list
// hygiene signal), and FBL complaints. Transient issues like rate-limiting,
// message-expired, and deferrals are NOT suppressed.
func (ing *Ingestor) routeToGlobalSuppression(rec AccountingRecord, isp ISP) {
	if rec.Recipient == "" {
		return
	}

	var reason, source string
	switch rec.Type {
	case "b": // bounce
		// DSN status is authoritative. NEVER suppress system/network/protocol/
		// security blocks (5.3/5.4/5.5/5.7): these are reputation/system
		// rejections of a VALID recipient (e.g. Charter/Spectrum "5.3.2 system
		// not accepting network messages"), which PMTA mislabels as
		// "bad-mailbox". Suppressing them permanently destroys valid subscribers
		// over a temporary sender-side block.
		if smtputil.IsReputationOrSystemBlock(rec.DSNStatus) {
			return
		}
		switch rec.BounceCat {
		case "bad-mailbox", "bad-domain", "inactive-mailbox", "quota-issues":
			// Genuine list-hygiene failures (invalid address / dead domain /
			// disabled or full mailbox). quota-issues is retained as a Yahoo
			// list-hygiene signal per product rule.
			reason = "hard_bounce"
			source = "pmta_bounce"
		default:
			// spam-related, policy-related, routing-errors, no-answer-from-host
			// and bad-connection are reputation/connection blocks — they do NOT
			// suppress the recipient.
			return
		}
	case "f": // FBL complaint
		reason = "spam_complaint"
		source = "pmta_fbl"
	default:
		return
	}

	ctx := context.Background()
	ing.globalHub.Suppress(ctx, rec.Recipient, reason, source, string(isp), rec.DSNStatus, rec.DSNDiag, rec.SourceIP, rec.JobID)
}

// persistToDB writes PMTA accounting events to mailing_tracking_events and
// updates mailing_campaigns counters. Since PMTA v5.0r7 doesn't support
// process-x-job, the jobId may be the envelope sender domain rather than
// the campaign UUID. We resolve the campaign by looking up the most recent
// 'sent' event for the recipient email.
func (ing *Ingestor) persistToDB(rec AccountingRecord, isp ISP) {
	if rec.Recipient == "" {
		return
	}

	var eventType string
	switch rec.Type {
	case "d":
		// PMTA "d" on an SES relay pool means "handed off to SES", not
		// "delivered to the recipient". Record it as relayed_to_ses so it is
		// NOT counted toward campaign delivered_count — the authoritative
		// delivery for SES-routed mail comes from the SES DELIVERY webhook.
		if IsPMTARelayedToSES(rec) {
			eventType = "relayed_to_ses"
		} else {
			eventType = "delivered"
		}
	case "b":
		eventType = "bounced"
	case "f":
		eventType = "complained"
	case "t", "tq":
		eventType = "deferred"
	default:
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	recipientEmail := strings.ToLower(strings.TrimSpace(rec.Recipient))
	recipientDomainRaw := ""
	if parts := strings.SplitN(recipientEmail, "@", 2); len(parts) == 2 {
		recipientDomainRaw = parts[1]
	}

	// Buffer every PMTA record to pmta_acct_raw BEFORE campaign resolution.
	// This table has no foreign keys and no complex lookups — it never drops records.
	_, bufErr := ing.db.ExecContext(ctx, `
		INSERT INTO pmta_acct_raw (record_type, recipient, sender, source_ip, vmta, pool,
			recipient_domain, bounce_cat, dsn_status, dsn_diag, job_id, time_logged)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
	`, rec.Type, recipientEmail, rec.Sender, rec.SourceIP, rec.VMTA, rec.Pool,
		recipientDomainRaw, rec.BounceCat, rec.DSNStatus, rec.DSNDiag, rec.JobID, rec.DeliveryTime)
	if bufErr != nil {
		log.Printf("[ingest-db] pmta_acct_raw buffer insert error: %v", bufErr)
	}

	// Resolve campaign_id: try jobId first (if it's a UUID), else look up by recipient
	campaignID := rec.JobID
	if !isUUID(campaignID) {
		var resolved sql.NullString
		ing.db.QueryRowContext(ctx, `
			SELECT campaign_id::text FROM mailing_message_log
			WHERE LOWER(email) = $1
			ORDER BY sent_at DESC LIMIT 1
		`, recipientEmail).Scan(&resolved)
		if resolved.Valid {
			campaignID = resolved.String
		} else {
			log.Printf("[ingest-db] no campaign found for recipient %s, skipping DB persist", rec.Recipient[:min(20, len(rec.Recipient))])
			return
		}
	}

	campUUID, err := uuid.Parse(campaignID)
	if err != nil {
		return
	}

	// Look up subscriber_id and organization_id via message_log (reliable
	// for multi-list campaigns where mailing_campaigns.list_id is NULL).
	var subscriberID, orgID sql.NullString
	ing.db.QueryRowContext(ctx, `
		SELECT subscriber_id::text, organization_id::text
		FROM mailing_message_log
		WHERE campaign_id = $1 AND LOWER(email) = $2
		ORDER BY sent_at DESC LIMIT 1
	`, campUUID, recipientEmail).Scan(&subscriberID, &orgID)

	// Fallback: get org_id directly from the campaign when message_log
	// didn't return it (e.g. older records without organization_id).
	if !orgID.Valid {
		ing.db.QueryRowContext(ctx, `
			SELECT organization_id::text FROM mailing_campaigns WHERE id = $1
		`, campUUID).Scan(&orgID)
	}

	if !orgID.Valid {
		log.Printf("[ingest-db] no organization_id for campaign %s, skipping", campUUID)
		return
	}

	dedupeSrc := fmt.Sprintf("pmta|%s|%s|%s|%s|%s|%s|%s",
		strings.ToLower(recipientEmail), campaignID, eventType, rec.Type, rec.BounceCat, rec.DSNStatus, rec.DeliveryTime)
	eventID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(dedupeSrc))
	var subIDPtr *uuid.UUID
	orgUUID, _ := uuid.Parse(orgID.String)
	orgIDPtr := &orgUUID

	if subscriberID.Valid {
		if u, e := uuid.Parse(subscriberID.String); e == nil {
			subIDPtr = &u
		}
	}

	// Extract sending domain from the envelope-sender address
	sendingDomain := ""
	if atIdx := strings.LastIndex(rec.Sender, "@"); atIdx >= 0 {
		sendingDomain = strings.ToLower(rec.Sender[atIdx+1:])
	}

	recipientDomain := ""
	if parts := strings.SplitN(recipientEmail, "@", 2); len(parts) == 2 {
		recipientDomain = strings.ToLower(parts[1])
	}

	sendingIP := rec.SourceIP
	if sendingIP == "" && rec.VMTA != "" {
		sendingIP = rec.VMTA
	}

	// event_at must be deterministic so drainer retries dedupe via ON CONFLICT.
	// PMTA's time_logged is the canonical event timestamp; fall back to NOW()
	// only when it's missing/unparseable. The PK on the partitioned table is
	// (id, event_at), so both columns must match for ON CONFLICT to fire.
	eventAt := parseDeliveryTime(rec.DeliveryTime)

	res, err := ing.db.ExecContext(ctx, `
		INSERT INTO mailing_tracking_events (id, organization_id, campaign_id, subscriber_id, event_type, bounce_type, bounce_reason, event_at, sending_domain, sending_ip, recipient_domain)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (id, event_at) DO NOTHING
	`, eventID, orgIDPtr, campUUID, subIDPtr, eventType, rec.BounceCat, rec.DSNStatus, eventAt, sendingDomain, sendingIP, recipientDomain)
	if err != nil {
		log.Printf("[ingest-db] tracking event insert error: %v", err)
		return
	}
	rowsAff, err := res.RowsAffected()
	if err != nil || rowsAff == 0 {
		return
	}

	// Phantom-bounce reconciliation:
	// When PMTA reports a real delivery, retroactively remove any prior
	// send_worker-written "bounced" event for the same (campaign,
	// subscriber) that came from a PMTA HTTP bridge submission error.
	// These are phantom bounces — PMTA never actually rejected the
	// message, the HTTP bridge just timed out / returned 5xx on first
	// submission and PMTA then delivered on retry. The counters are
	// decremented to match.
	if eventType == "delivered" && subIDPtr != nil {
		var phantomCount int64
		if err := ing.db.QueryRowContext(ctx, `
			WITH deleted AS (
				DELETE FROM mailing_tracking_events
				WHERE campaign_id = $1
				  AND subscriber_id = $2
				  AND event_type = 'bounced'
				  AND event_at > NOW() - INTERVAL '24 hours'
				  AND bounce_reason LIKE 'PMTA API error%'
				RETURNING bounce_type
			)
			SELECT COUNT(*) FROM deleted
		`, campUUID, *subIDPtr).Scan(&phantomCount); err == nil && phantomCount > 0 {
			ing.db.ExecContext(ctx, `
				UPDATE mailing_campaigns
				SET bounce_count = GREATEST(COALESCE(bounce_count, 0) - $2, 0),
				    soft_bounce_count = GREATEST(COALESCE(soft_bounce_count, 0) - $2, 0),
				    updated_at = NOW()
				WHERE id = $1
			`, campUUID, phantomCount)
			log.Printf("[ingest-db] reconciled %d phantom bounce(s) for campaign=%s sub=%s (delivered after bridge-error)",
				phantomCount, campUUID, *subIDPtr)
		}
	}

	// Update campaign aggregate counters.
	hardBounce := false
	switch eventType {
	case "delivered":
		ing.db.ExecContext(ctx, `UPDATE mailing_campaigns SET delivered_count = COALESCE(delivered_count, 0) + 1, updated_at = NOW() WHERE id = $1`, campUUID)
	case "bounced":
		ing.db.ExecContext(ctx, `UPDATE mailing_campaigns SET bounce_count = COALESCE(bounce_count, 0) + 1, updated_at = NOW() WHERE id = $1`, campUUID)
		// DSN-aware: reputation/system blocks (5.3/5.4/5.5/5.7) and connection
		// failures are counted as soft, not hard, so the campaign's
		// hard_bounce_count reflects true list hygiene — not ISP reputation
		// rejections that PMTA mislabels as bad-mailbox.
		hardBounce = smtputil.ClassifyBounce(rec.DSNStatus, rec.BounceCat, rec.DSNDiag) == smtputil.ClassHard
		if hardBounce {
			ing.db.ExecContext(ctx, `UPDATE mailing_campaigns SET hard_bounce_count = COALESCE(hard_bounce_count, 0) + 1 WHERE id = $1`, campUUID)
		} else {
			ing.db.ExecContext(ctx, `UPDATE mailing_campaigns SET soft_bounce_count = COALESCE(soft_bounce_count, 0) + 1 WHERE id = $1`, campUUID)
		}
	case "complained":
		ing.db.ExecContext(ctx, `UPDATE mailing_campaigns SET complaint_count = COALESCE(complaint_count, 0) + 1, updated_at = NOW() WHERE id = $1`, campUUID)
	case "deferred":
		// Deferrals live in mailing_tracking_events only; no campaign counter.
	}

	// Engagement telemetry bridge: increment Redis counters for delivered/complained
	ing.incrEngagementCounters(isp, eventType)

	// Bounce rate monitoring: track hard bounces in a sliding window
	// and alert when threshold is exceeded.
	if ing.alerter != nil && ing.bounceWin != nil && (eventType == "delivered" || eventType == "bounced") {
		campName := campaignID[:min(8, len(campaignID))]
		rate, exceeded := ing.bounceWin.record(campaignID, campName, eventType, hardBounce)
		if exceeded {
			ing.alerter.SendBounceRateAlert(campaignID, campName, rate, hardBounceRateThreshold)
		}
	}

	// Update IP address counters when we can resolve the source IP.
	if sendingIP != "" && (eventType == "delivered" || eventType == "bounced") {
		switch eventType {
		case "delivered":
			ing.db.ExecContext(ctx, `
				UPDATE mailing_ip_addresses SET total_delivered = COALESCE(total_delivered, 0) + 1, updated_at = NOW()
				WHERE ip_address = $1::inet`, sendingIP)
		case "bounced":
			ing.db.ExecContext(ctx, `
				UPDATE mailing_ip_addresses SET total_bounced = COALESCE(total_bounced, 0) + 1, updated_at = NOW()
				WHERE ip_address = $1::inet`, sendingIP)
		}
	}

	// Enrich inbox profiles with delivery/bounce/complaint data.
	if eventType == "delivered" || eventType == "bounced" || eventType == "complained" {
		domain := ""
		if parts := strings.SplitN(recipientEmail, "@", 2); len(parts) == 2 {
			domain = parts[1]
		}
		eHash := ingestEmailHash(recipientEmail)
		switch eventType {
		case "delivered":
			ing.db.ExecContext(ctx, `
				INSERT INTO mailing_inbox_profiles (id, email_hash, email, domain, total_sends, last_send_at, first_seen_at, updated_at)
				VALUES (gen_random_uuid(), $1, $2, $3, 1, NOW(), NOW(), NOW())
				ON CONFLICT (email_hash) DO UPDATE SET
					total_sends = COALESCE(mailing_inbox_profiles.total_sends, 0) + 1,
					last_send_at = NOW(), updated_at = NOW()
			`, eHash, recipientEmail, domain)
		case "bounced":
			ing.db.ExecContext(ctx, `
				INSERT INTO mailing_inbox_profiles (id, email_hash, email, domain, total_bounces, last_bounce_at, first_seen_at, updated_at)
				VALUES (gen_random_uuid(), $1, $2, $3, 1, NOW(), NOW(), NOW())
				ON CONFLICT (email_hash) DO UPDATE SET
					total_bounces = COALESCE(mailing_inbox_profiles.total_bounces, 0) + 1,
					last_bounce_at = NOW(), updated_at = NOW()
			`, eHash, recipientEmail, domain)
		case "complained":
			ing.db.ExecContext(ctx, `
				INSERT INTO mailing_inbox_profiles (id, email_hash, email, domain, total_complaints, first_seen_at, updated_at)
				VALUES (gen_random_uuid(), $1, $2, $3, 1, NOW(), NOW())
				ON CONFLICT (email_hash) DO UPDATE SET
					total_complaints = COALESCE(mailing_inbox_profiles.total_complaints, 0) + 1,
					updated_at = NOW()
			`, eHash, recipientEmail, domain)
		}
		ing.recomputeProfileScore(ctx, eHash)
	}
}

func (ing *Ingestor) recomputeProfileScore(ctx context.Context, eHash string) {
	var totalSends, totalOpens, totalClicks int
	var lastOpen *time.Time
	err := ing.db.QueryRowContext(ctx, `
		SELECT COALESCE(total_sends,0), COALESCE(total_opens,0), COALESCE(total_clicks,0), last_open_at
		FROM mailing_inbox_profiles WHERE email_hash = $1
	`, eHash).Scan(&totalSends, &totalOpens, &totalClicks, &lastOpen)
	if err != nil || totalSends == 0 {
		return
	}

	openRate := float64(totalOpens) / float64(totalSends)
	clickRate := float64(totalClicks) / float64(totalSends)
	score := (openRate * 0.6) + (clickRate * 0.4)

	if lastOpen != nil {
		daysSince := time.Since(*lastOpen).Hours() / 24
		if daysSince < 7 {
			score += 0.20
		} else if daysSince < 30 {
			score += 0.10
		}
	}
	if score > 1.0 {
		score = 1.0
	}

	ing.db.ExecContext(ctx, `
		UPDATE mailing_inbox_profiles SET engagement_score = $2, updated_at = NOW()
		WHERE email_hash = $1
	`, eHash, score)
}

// incrEngagementCounters pushes delivered and complained counts into Redis
// for the engagement telemetry bridge. Fire-and-forget with short timeout.
func (ing *Ingestor) incrEngagementCounters(isp ISP, eventType string) {
	if ing.redisClient == nil || isp == "" {
		return
	}

	var field string
	switch eventType {
	case "delivered":
		field = EngFieldDelivered
	case "complained":
		field = EngFieldComplaints
	default:
		return
	}

	now := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	pipe := ing.redisClient.Pipeline()

	key1h := EngagementRedisKey1h(isp, now)
	pipe.HIncrBy(ctx, key1h, field, 1)
	pipe.Expire(ctx, key1h, EngagementTTL1h)

	if eventType == "complained" {
		key5m := EngagementRedisKey5m(isp, now)
		pipe.HIncrBy(ctx, key5m, field, 1)
		pipe.Expire(ctx, key5m, EngagementTTL5m)
	}

	if _, err := pipe.Exec(ctx); err != nil {
		log.Printf("[engagement-redis] incr %s/%s error: %v", isp, field, err)
	}
}

// isHardBounceCategory returns true for PMTA bounce categories that indicate a
// permanent list-hygiene failure (invalid address / dead domain / disabled
// mailbox). This is a category-only fallback; prefer smtputil.ClassifyBounce
// which also consults the authoritative DSN status. Reputation/system blocks
// (spam-related, policy-related, routing-errors) and connection failures
// (no-answer-from-host, bad-connection) are deliberately NOT hard.
// Must stay in sync with api.HardBounceCategories.
func isHardBounceCategory(cat string) bool {
	switch cat {
	case "hard", "bad-mailbox", "bad-domain", "inactive-mailbox":
		return true
	default:
		return false
	}
}

func isUUID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
}

// parseDeliveryTime converts PMTA's time_logged string to a time.Time. PMTA
// emits the format "2006-01-02 15:04:05-0700" by default, but operators can
// configure ISO 8601 or unix-epoch output. We try the common formats in
// order; if none match we fall back to time.Now() so an INSERT still lands.
//
// Determinism is required: the deterministic event UUID embeds the raw
// time_logged string, so re-shipped records hit the same (id, event_at)
// tuple and ON CONFLICT (id, event_at) DO NOTHING dedupes them.
func parseDeliveryTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Now().UTC()
	}
	for _, layout := range []string{
		"2006-01-02 15:04:05-0700",
		"2006-01-02 15:04:05Z07:00",
		"2006-01-02 15:04:05",
		time.RFC3339Nano,
		time.RFC3339,
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	if epoch, err := strconv.ParseInt(s, 10, 64); err == nil && epoch > 1_000_000_000 {
		return time.Unix(epoch, 0).UTC()
	}
	return time.Now().UTC()
}

func sanitizeJSON(s string) string {
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", "")
	if len(s) > 500 {
		s = s[:500]
	}
	return s
}

func (ing *Ingestor) classifyRecord(rec AccountingRecord) ISP {
	if rec.Domain != "" {
		isp := ing.registry.ClassifyDomain(rec.Domain)
		if isp != "" {
			return isp
		}
	}
	if rec.Recipient != "" {
		return ing.registry.ClassifyEmail(rec.Recipient)
	}
	return ""
}

// StartPolling begins periodically polling the PMTA management API.
func (ing *Ingestor) StartPolling(ctx context.Context) {
	if ing.pmtaHost == "" {
		log.Println("[ingest] PMTA polling disabled: no host configured")
		return
	}

	go func() {
		ticker := time.NewTicker(ing.pollInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				ing.pollPMTAStatus(ctx)
			}
		}
	}()
}

func (ing *Ingestor) pollPMTAStatus(ctx context.Context) {
	endpoints := []string{"status", "queues", "vmtas", "domains"}
	for _, ep := range endpoints {
		url := fmt.Sprintf("http://%s:%d/%s?format=json", ing.pmtaHost, ing.pmtaPort, ep)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			continue
		}
		if ing.pmtaUser != "" || ing.pmtaPassword != "" {
			req.SetBasicAuth(ing.pmtaUser, ing.pmtaPassword)
		}
		resp, err := ing.httpClient.Do(req)
		if err != nil {
			log.Printf("[ingest] PMTA poll %s error: %v", ep, err)
			continue
		}
		resp.Body.Close()
	}
}
