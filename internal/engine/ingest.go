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
		if isp == "" {
			continue
		}

		// Feed signal processor
		ing.processor.Ingest(isp, rec)

		// Notify ISP-specific agent listeners
		for _, ch := range ing.listeners[isp] {
			select {
			case ch <- rec:
			default: // drop if channel full
			}
		}

		// Feed campaign event tracker
		if ing.tracker != nil && rec.JobID != "" {
			ing.routeToCampaignTracker(rec, isp)
		}

		// Feed global suppression hub for ALL negative signals
		if ing.globalHub != nil {
			ing.routeToGlobalSuppression(rec, isp)
		}

		// Persist to mailing_tracking_events and update campaign counters
		if ing.db != nil {
			ing.persistToDB(rec, isp)
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

// routeToGlobalSuppression sends any negative signal to the global hub.
// Hard bounces, complaints, and repeated transients all result in immediate
// global suppression — ISP-agnostic, zero tolerance.
func (ing *Ingestor) routeToGlobalSuppression(rec AccountingRecord, isp ISP) {
	if rec.Recipient == "" {
		return
	}

	var reason, source string
	switch rec.Type {
	case "b": // bounce
		reason = "hard_bounce"
		if rec.BounceCat == "quota-issues" || rec.BounceCat == "no-answer-from-host" {
			reason = "soft_bounce"
		}
		source = "pmta_bounce"
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
		eventType = ClassifyBounce(rec.BounceCat)
	case "f":
		eventType = "complained"
	default:
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Resolve campaign_id: try jobId first (if it's a UUID), else look up by recipient
	campaignID := rec.JobID
	if !isUUID(campaignID) {
		var resolved sql.NullString
		ing.db.QueryRowContext(ctx, `
			SELECT campaign_id::text FROM mailing_tracking_events
			WHERE LOWER(email) = LOWER($1) AND event_type = 'sent'
			ORDER BY event_at DESC LIMIT 1
		`, strings.TrimSpace(rec.Recipient)).Scan(&resolved)
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

	// Look up subscriber_id and organization_id from campaign + subscribers
	recipientEmail := strings.ToLower(strings.TrimSpace(rec.Recipient))
	var subscriberID, orgID sql.NullString
	ing.db.QueryRowContext(ctx, `
		SELECT e.subscriber_id::text, c.organization_id::text 
		FROM mailing_tracking_events e
		JOIN mailing_subscribers s ON s.id = e.subscriber_id
		JOIN mailing_campaigns c ON c.id = e.campaign_id
		WHERE e.campaign_id = $1 AND LOWER(s.email) = $2 AND e.event_type = 'sent'
		LIMIT 1
	`, campUUID, recipientEmail).Scan(&subscriberID, &orgID)

	eventID := uuid.New()
	var subIDPtr, orgIDPtr *uuid.UUID
	if subscriberID.Valid {
		if u, e := uuid.Parse(subscriberID.String); e == nil {
			subIDPtr = &u
		}
	}
	if orgID.Valid {
		if u, e := uuid.Parse(orgID.String); e == nil {
			orgIDPtr = &u
		}
	}

	_, err = ing.db.ExecContext(ctx, `
		INSERT INTO mailing_tracking_events (id, organization_id, campaign_id, subscriber_id, event_type, bounce_type, bounce_reason, event_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())
		ON CONFLICT DO NOTHING
	`, eventID, orgIDPtr, campUUID, subIDPtr, eventType, rec.BounceCat, rec.DSNStatus)
	if err != nil {
		log.Printf("[ingest-db] tracking event insert error: %v", err)
	}

	// Update campaign aggregate counters
	var counterCol string
	switch eventType {
	case "delivered":
		counterCol = "delivered_count"
	case "hard_bounce":
		counterCol = "bounce_count"
	case "soft_bounce":
		counterCol = "bounce_count"
	case "complained":
		counterCol = "complaint_count"
	}
	if counterCol != "" {
		ing.db.ExecContext(ctx, fmt.Sprintf(`
			UPDATE mailing_campaigns SET %s = COALESCE(%s, 0) + 1, updated_at = NOW() WHERE id = $1
		`, counterCol, counterCol), campUUID)
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
