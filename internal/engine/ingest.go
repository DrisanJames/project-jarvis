package engine

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Ingestor receives PMTA accounting records via webhook and polls PMTA
// status APIs. It classifies each record by ISP and fans out to the
// SignalProcessor, agent clusters, CampaignEventTracker, and
// GlobalSuppressionHub.
type Ingestor struct {
	registry  *ISPRegistry
	processor *SignalProcessor
	tracker   *CampaignEventTracker
	globalHub *GlobalSuppressionHub
	db        *sql.DB

	// Record listeners (agents subscribe to their ISP's records)
	listeners map[ISP][]chan<- AccountingRecord

	// PMTA management API polling
	pmtaHost     string
	pmtaPort     int
	pmtaUser     string
	pmtaPassword string
	pollInterval time.Duration
	httpClient   *http.Client
}

// IngestorConfig holds configuration for the ingestor.
type IngestorConfig struct {
	PMTAHost     string
	PMTAPort     int
	PMTAUser     string
	PMTAPassword string
	PollInterval time.Duration
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

// SubscribeISP registers a listener for records classified to a specific ISP.
func (ing *Ingestor) SubscribeISP(isp ISP, ch chan<- AccountingRecord) {
	ing.listeners[isp] = append(ing.listeners[isp], ch)
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
		// Try single record
		var single AccountingRecord
		if err2 := json.Unmarshal(body, &single); err2 == nil {
			records = []AccountingRecord{single}
		} else {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
	}

	processed := 0
	for _, rec := range records {
		isp := ing.classifyRecord(rec)

		// Global suppression and DB persistence fire for ALL records,
		// even if the domain doesn't map to a known ISP.
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

		// ISP-specific processing only for classified domains
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

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"received":  len(records),
		"processed": processed,
	})
}

func (ing *Ingestor) routeToCampaignTracker(rec AccountingRecord, isp ISP) {
	var eventType string
	switch rec.Type {
	case "d":
		eventType = "delivered"
	case "b":
		eventType = ClassifyBounce(rec.BounceCat)
	case "t", "tq":
		return // transients are not campaign-level events
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
}

// routeToGlobalSuppression suppresses only permanent failures: hard bounces
// (bad-mailbox, bad-domain, inactive-mailbox) and FBL complaints. Transient
// issues like quota, throttling, and deferrals are NOT suppressed — they are
// temporary and the recipient should be retried on future campaigns.
func (ing *Ingestor) routeToGlobalSuppression(rec AccountingRecord, isp ISP) {
	if rec.Recipient == "" {
		return
	}

	var reason, source string
	switch rec.Type {
	case "b": // bounce
		switch rec.BounceCat {
		case "bad-mailbox", "bad-domain", "inactive-mailbox", "quota-issues":
			reason = "hard_bounce"
			source = "pmta_bounce"
		default:
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
		eventType = "delivered"
	case "b":
		eventType = "bounced"
	case "f":
		eventType = "complained"
	default:
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Resolve campaign_id: try jobId first (if it's a UUID), else look up by recipient
	recipientEmail := strings.ToLower(strings.TrimSpace(rec.Recipient))
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

	// Look up subscriber_id and organization_id from campaign list membership
	var subscriberID, orgID sql.NullString
	ing.db.QueryRowContext(ctx, `
		SELECT s.id::text, c.organization_id::text
		FROM mailing_subscribers s
		JOIN mailing_campaigns c ON c.list_id = s.list_id AND c.id = $1
		WHERE LOWER(s.email) = $2
		LIMIT 1
	`, campUUID, recipientEmail).Scan(&subscriberID, &orgID)

	// If subscriber lookup didn't yield org_id, get it directly from the campaign
	if !orgID.Valid {
		ing.db.QueryRowContext(ctx, `
			SELECT organization_id::text FROM mailing_campaigns WHERE id = $1
		`, campUUID).Scan(&orgID)
	}

	if !orgID.Valid {
		log.Printf("[ingest-db] no organization_id for campaign %s, skipping", campUUID)
		return
	}

	eventID := uuid.New()
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

	_, err = ing.db.ExecContext(ctx, `
		INSERT INTO mailing_tracking_events (id, organization_id, campaign_id, subscriber_id, event_type, bounce_type, bounce_reason, event_at, sending_domain, sending_ip, recipient_domain)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NOW(), $8, $9, $10)
		ON CONFLICT DO NOTHING
	`, eventID, orgIDPtr, campUUID, subIDPtr, eventType, rec.BounceCat, rec.DSNStatus, sendingDomain, sendingIP, recipientDomain)
	if err != nil {
		log.Printf("[ingest-db] tracking event insert error: %v", err)
	}

	// Update campaign aggregate counters
	switch eventType {
	case "delivered":
		ing.db.ExecContext(ctx, `UPDATE mailing_campaigns SET delivered_count = COALESCE(delivered_count, 0) + 1, updated_at = NOW() WHERE id = $1`, campUUID)
	case "hard_bounce":
		ing.db.ExecContext(ctx, `UPDATE mailing_campaigns SET bounce_count = COALESCE(bounce_count, 0) + 1, hard_bounce_count = COALESCE(hard_bounce_count, 0) + 1, updated_at = NOW() WHERE id = $1`, campUUID)
	case "soft_bounce":
		ing.db.ExecContext(ctx, `UPDATE mailing_campaigns SET bounce_count = COALESCE(bounce_count, 0) + 1, soft_bounce_count = COALESCE(soft_bounce_count, 0) + 1, updated_at = NOW() WHERE id = $1`, campUUID)
	case "complained":
		ing.db.ExecContext(ctx, `UPDATE mailing_campaigns SET complaint_count = COALESCE(complaint_count, 0) + 1, updated_at = NOW() WHERE id = $1`, campUUID)
	}

	// Enrich inbox profiles with delivery/bounce data from PMTA webhook
	if eventType == "delivered" || eventType == "hard_bounce" || eventType == "soft_bounce" {
		domain := ""
		if parts := strings.SplitN(recipientEmail, "@", 2); len(parts) == 2 {
			domain = parts[1]
		}
		if eventType == "delivered" {
			ing.db.ExecContext(ctx, `
				INSERT INTO mailing_inbox_profiles (id, email, domain, total_sent, last_sent_at, created_at, updated_at)
				VALUES (gen_random_uuid(), $1, $2, 1, NOW(), NOW(), NOW())
				ON CONFLICT (email) DO UPDATE SET total_sent = mailing_inbox_profiles.total_sent + 1, last_sent_at = NOW(), updated_at = NOW()
			`, recipientEmail, domain)
		} else {
			ing.db.ExecContext(ctx, `
				INSERT INTO mailing_inbox_profiles (id, email, domain, total_bounces, last_bounce_at, created_at, updated_at)
				VALUES (gen_random_uuid(), $1, $2, 1, NOW(), NOW(), NOW())
				ON CONFLICT (email) DO UPDATE SET total_bounces = COALESCE(mailing_inbox_profiles.total_bounces, 0) + 1, last_bounce_at = NOW(), updated_at = NOW()
			`, recipientEmail, domain)
		}
	}
}

func isUUID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
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
		url := fmt.Sprintf("https://%s:%d/%s?format=json", ing.pmtaHost, ing.pmtaPort, ep)
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
