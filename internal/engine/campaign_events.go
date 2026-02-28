package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// CampaignEventTracker aggregates all delivery and engagement events per
// campaign, computes unique counts, detects inactive emails, and periodically
// flushes campaign reports to S3 as JSON.
// InactiveCallback is invoked when a recipient is flagged as inactive
// (sent N+ times with zero engagement). Propagates to global suppression.
type InactiveCallback func(email, campaignID string)

type CampaignEventTracker struct {
	s3Client *s3.Client
	bucket   string

	mu        sync.Mutex
	campaigns map[string]*CampaignMetrics

	inactiveThreshold int
	flushInterval     time.Duration
	stopCh            chan struct{}

	onInactive InactiveCallback

	subMu       sync.RWMutex
	subscribers map[string]chan CampaignEvent
}

// CampaignMetrics holds all aggregated metrics for a single campaign.
type CampaignMetrics struct {
	CampaignID string    `json:"campaign_id"`
	StartedAt  time.Time `json:"started_at"`
	UpdatedAt  time.Time `json:"updated_at"`

	Sent       int `json:"sent"`
	Delivered  int `json:"delivered"`
	SoftBounce int `json:"soft_bounce"`
	HardBounce int `json:"hard_bounce"`
	Complaints int `json:"complaints"`
	Unsubscribes int `json:"unsubscribes"`

	Opens       int `json:"opens"`
	UniqueOpens int `json:"unique_opens"`
	Clicks      int `json:"clicks"`
	UniqueClicks int `json:"unique_clicks"`

	Inactive int `json:"inactive"`

	// Tracking sets for uniqueness
	openedRecipients  map[string]bool `json:"-"`
	clickedRecipients map[string]bool `json:"-"`

	// Per-recipient engagement tracking: email -> send count without engagement
	recipientSends map[string]int  `json:"-"`
	recipientEngaged map[string]bool `json:"-"`
	inactiveSet      map[string]bool `json:"-"`

	// Per-ISP breakdown
	ISPMetrics map[string]*ISPCampaignMetrics `json:"isp_metrics"`

	// Per-variant breakdown
	VariantMetrics map[string]*VariantCampaignMetrics `json:"variant_metrics"`

	// Event log for recent events
	recentEvents []CampaignEvent `json:"-"`
}

// ISPCampaignMetrics tracks metrics per ISP within a campaign.
type ISPCampaignMetrics struct {
	ISP          string `json:"isp"`
	Sent         int    `json:"sent"`
	Delivered    int    `json:"delivered"`
	SoftBounce   int    `json:"soft_bounce"`
	HardBounce   int    `json:"hard_bounce"`
	Complaints   int    `json:"complaints"`
	Opens        int    `json:"opens"`
	UniqueOpens  int    `json:"unique_opens"`
	Clicks       int    `json:"clicks"`
	UniqueClicks int    `json:"unique_clicks"`

	openedRecipients  map[string]bool `json:"-"`
	clickedRecipients map[string]bool `json:"-"`
}

// VariantCampaignMetrics tracks metrics per A/B variant within a campaign.
type VariantCampaignMetrics struct {
	Variant      string `json:"variant"`
	Sent         int    `json:"sent"`
	Delivered    int    `json:"delivered"`
	Opens        int    `json:"opens"`
	UniqueOpens  int    `json:"unique_opens"`
	Clicks       int    `json:"clicks"`
	UniqueClicks int    `json:"unique_clicks"`

	openedRecipients  map[string]bool `json:"-"`
	clickedRecipients map[string]bool `json:"-"`
}

// CampaignEvent is a single tracked event flowing through the system.
type CampaignEvent struct {
	CampaignID string    `json:"campaign_id"`
	EventType  string    `json:"event_type"`
	Recipient  string    `json:"recipient"`
	ISP        string    `json:"isp,omitempty"`
	Variant    string    `json:"variant,omitempty"`
	SourceIP   string    `json:"source_ip,omitempty"`
	LinkURL    string    `json:"link_url,omitempty"`
	BounceType string    `json:"bounce_type,omitempty"`
	DSNCode    string    `json:"dsn_code,omitempty"`
	DSNDiag    string    `json:"dsn_diag,omitempty"`
	Timestamp  time.Time `json:"timestamp"`
}

// CampaignReport is the S3-persisted JSON report for a campaign.
type CampaignReport struct {
	CampaignID string    `json:"campaign_id"`
	GeneratedAt time.Time `json:"generated_at"`

	Summary CampaignReportSummary `json:"summary"`

	ISPBreakdown     map[string]*ISPCampaignMetrics     `json:"isp_breakdown"`
	VariantBreakdown map[string]*VariantCampaignMetrics  `json:"variant_breakdown"`

	InactiveEmails []string `json:"inactive_emails,omitempty"`

	Rates CampaignRates `json:"rates"`
}

// CampaignReportSummary holds the top-level metrics.
type CampaignReportSummary struct {
	Sent         int `json:"sent"`
	Delivered    int `json:"delivered"`
	SoftBounce   int `json:"soft_bounce"`
	HardBounce   int `json:"hard_bounce"`
	Complaints   int `json:"complaints"`
	Unsubscribes int `json:"unsubscribes"`
	Opens        int `json:"opens"`
	UniqueOpens  int `json:"unique_opens"`
	Clicks       int `json:"clicks"`
	UniqueClicks int `json:"unique_clicks"`
	Inactive     int `json:"inactive"`
}

// CampaignRates holds computed rate percentages.
type CampaignRates struct {
	DeliveryRate   float64 `json:"delivery_rate"`
	BounceRate     float64 `json:"bounce_rate"`
	OpenRate       float64 `json:"open_rate"`
	ClickRate      float64 `json:"click_rate"`
	CTR            float64 `json:"click_through_rate"`
	ComplaintRate  float64 `json:"complaint_rate"`
	UnsubRate      float64 `json:"unsubscribe_rate"`
	InactiveRate   float64 `json:"inactive_rate"`
}

// NewCampaignEventTracker creates a new tracker.
func NewCampaignEventTracker(s3Client *s3.Client, bucket string) *CampaignEventTracker {
	t := &CampaignEventTracker{
		s3Client:          s3Client,
		bucket:            bucket,
		campaigns:         make(map[string]*CampaignMetrics),
		inactiveThreshold: 4,
		flushInterval:     5 * time.Minute,
		stopCh:            make(chan struct{}),
		subscribers:       make(map[string]chan CampaignEvent),
	}
	go t.flushLoop()
	return t
}

// SetInactiveCallback registers a callback that fires when a recipient is
// flagged as inactive. This bridges to the GlobalSuppressionHub.
func (t *CampaignEventTracker) SetInactiveCallback(cb InactiveCallback) {
	t.onInactive = cb
}

func (t *CampaignEventTracker) flushLoop() {
	ticker := time.NewTicker(t.flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			t.FlushAll(context.Background())
		case <-t.stopCh:
			return
		}
	}
}

// Stop terminates the background flush loop.
func (t *CampaignEventTracker) Stop() {
	close(t.stopCh)
}

// Subscribe registers an SSE client for real-time campaign events.
func (t *CampaignEventTracker) Subscribe(id string) <-chan CampaignEvent {
	ch := make(chan CampaignEvent, 200)
	t.subMu.Lock()
	t.subscribers[id] = ch
	t.subMu.Unlock()
	return ch
}

// Unsubscribe removes an SSE client.
func (t *CampaignEventTracker) Unsubscribe(id string) {
	t.subMu.Lock()
	if ch, ok := t.subscribers[id]; ok {
		close(ch)
		delete(t.subscribers, id)
	}
	t.subMu.Unlock()
}

func (t *CampaignEventTracker) fanOut(e CampaignEvent) {
	t.subMu.RLock()
	defer t.subMu.RUnlock()
	for _, ch := range t.subscribers {
		select {
		case ch <- e:
		default:
		}
	}
}

func (t *CampaignEventTracker) ensureCampaign(id string) *CampaignMetrics {
	if m, ok := t.campaigns[id]; ok {
		return m
	}
	m := &CampaignMetrics{
		CampaignID:       id,
		StartedAt:        time.Now(),
		openedRecipients: make(map[string]bool),
		clickedRecipients: make(map[string]bool),
		recipientSends:   make(map[string]int),
		recipientEngaged: make(map[string]bool),
		inactiveSet:      make(map[string]bool),
		ISPMetrics:       make(map[string]*ISPCampaignMetrics),
		VariantMetrics:   make(map[string]*VariantCampaignMetrics),
	}
	t.campaigns[id] = m
	return m
}

func (m *CampaignMetrics) ensureISP(isp string) *ISPCampaignMetrics {
	if isp == "" {
		return nil
	}
	if im, ok := m.ISPMetrics[isp]; ok {
		return im
	}
	im := &ISPCampaignMetrics{
		ISP:               isp,
		openedRecipients:  make(map[string]bool),
		clickedRecipients: make(map[string]bool),
	}
	m.ISPMetrics[isp] = im
	return im
}

func (m *CampaignMetrics) ensureVariant(v string) *VariantCampaignMetrics {
	if v == "" {
		return nil
	}
	if vm, ok := m.VariantMetrics[v]; ok {
		return vm
	}
	vm := &VariantCampaignMetrics{
		Variant:           v,
		openedRecipients:  make(map[string]bool),
		clickedRecipients: make(map[string]bool),
	}
	m.VariantMetrics[v] = vm
	return vm
}

// RecordEvent records a campaign event and updates all aggregations.
func (t *CampaignEventTracker) RecordEvent(e CampaignEvent) {
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}

	t.mu.Lock()
	m := t.ensureCampaign(e.CampaignID)
	m.UpdatedAt = e.Timestamp

	ispM := m.ensureISP(e.ISP)
	varM := m.ensureVariant(e.Variant)

	switch e.EventType {
	case "sent":
		m.Sent++
		if ispM != nil {
			ispM.Sent++
		}
		if varM != nil {
			varM.Sent++
		}
		if e.Recipient != "" {
			m.recipientSends[e.Recipient]++
		}

	case "delivered":
		m.Delivered++
		if ispM != nil {
			ispM.Delivered++
		}
		if varM != nil {
			varM.Delivered++
		}

	case "soft_bounce":
		m.SoftBounce++
		if ispM != nil {
			ispM.SoftBounce++
		}

	case "hard_bounce":
		m.HardBounce++
		if ispM != nil {
			ispM.HardBounce++
		}

	case "complaint":
		m.Complaints++
		if ispM != nil {
			ispM.Complaints++
		}

	case "unsubscribe":
		m.Unsubscribes++

	case "open":
		m.Opens++
		if ispM != nil {
			ispM.Opens++
		}
		if varM != nil {
			varM.Opens++
		}
		if e.Recipient != "" {
			if !m.openedRecipients[e.Recipient] {
				m.openedRecipients[e.Recipient] = true
				m.UniqueOpens++
				if ispM != nil && !ispM.openedRecipients[e.Recipient] {
					ispM.openedRecipients[e.Recipient] = true
					ispM.UniqueOpens++
				}
				if varM != nil && !varM.openedRecipients[e.Recipient] {
					varM.openedRecipients[e.Recipient] = true
					varM.UniqueOpens++
				}
			}
			m.recipientEngaged[e.Recipient] = true
		}

	case "click":
		m.Clicks++
		if ispM != nil {
			ispM.Clicks++
		}
		if varM != nil {
			varM.Clicks++
		}
		if e.Recipient != "" {
			if !m.clickedRecipients[e.Recipient] {
				m.clickedRecipients[e.Recipient] = true
				m.UniqueClicks++
				if ispM != nil && !ispM.clickedRecipients[e.Recipient] {
					ispM.clickedRecipients[e.Recipient] = true
					ispM.UniqueClicks++
				}
				if varM != nil && !varM.clickedRecipients[e.Recipient] {
					varM.clickedRecipients[e.Recipient] = true
					varM.UniqueClicks++
				}
			}
			m.recipientEngaged[e.Recipient] = true
		}
	}

	// Detect inactive: sent N times with no engagement
	if e.EventType == "sent" && e.Recipient != "" {
		sends := m.recipientSends[e.Recipient]
		engaged := m.recipientEngaged[e.Recipient]
		if sends >= t.inactiveThreshold && !engaged && !m.inactiveSet[e.Recipient] {
			m.inactiveSet[e.Recipient] = true
			m.Inactive++

			// Auto-suppress globally — zero tolerance for inactive addresses
			if t.onInactive != nil {
				go t.onInactive(e.Recipient, e.CampaignID)
			}
		}
	}

	// Keep recent events (capped at 1000)
	m.recentEvents = append(m.recentEvents, e)
	if len(m.recentEvents) > 1000 {
		m.recentEvents = m.recentEvents[len(m.recentEvents)-1000:]
	}
	t.mu.Unlock()

	t.fanOut(e)
}

// GetMetrics returns the current metrics for a campaign.
func (t *CampaignEventTracker) GetMetrics(campaignID string) *CampaignMetrics {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.campaigns[campaignID]
}

// GetAllCampaigns returns a summary of all active campaigns.
func (t *CampaignEventTracker) GetAllCampaigns() []CampaignMetrics {
	t.mu.Lock()
	defer t.mu.Unlock()
	var result []CampaignMetrics
	for _, m := range t.campaigns {
		result = append(result, *m)
	}
	return result
}

// GetReport generates a full campaign report.
func (t *CampaignEventTracker) GetReport(campaignID string) *CampaignReport {
	t.mu.Lock()
	m, ok := t.campaigns[campaignID]
	if !ok {
		t.mu.Unlock()
		return nil
	}

	var inactiveEmails []string
	for email := range m.inactiveSet {
		inactiveEmails = append(inactiveEmails, email)
	}

	report := &CampaignReport{
		CampaignID:  campaignID,
		GeneratedAt: time.Now(),
		Summary: CampaignReportSummary{
			Sent:         m.Sent,
			Delivered:    m.Delivered,
			SoftBounce:   m.SoftBounce,
			HardBounce:   m.HardBounce,
			Complaints:   m.Complaints,
			Unsubscribes: m.Unsubscribes,
			Opens:        m.Opens,
			UniqueOpens:  m.UniqueOpens,
			Clicks:       m.Clicks,
			UniqueClicks: m.UniqueClicks,
			Inactive:     m.Inactive,
		},
		ISPBreakdown:     m.ISPMetrics,
		VariantBreakdown: m.VariantMetrics,
		InactiveEmails:   inactiveEmails,
	}

	sent := float64(m.Sent)
	delivered := float64(m.Delivered)
	if sent > 0 {
		report.Rates.DeliveryRate = delivered / sent * 100
		report.Rates.BounceRate = float64(m.SoftBounce+m.HardBounce) / sent * 100
		report.Rates.ComplaintRate = float64(m.Complaints) / sent * 100
		report.Rates.UnsubRate = float64(m.Unsubscribes) / sent * 100
		report.Rates.InactiveRate = float64(m.Inactive) / sent * 100
	}
	if delivered > 0 {
		report.Rates.OpenRate = float64(m.UniqueOpens) / delivered * 100
		report.Rates.ClickRate = float64(m.UniqueClicks) / delivered * 100
	}
	if m.UniqueOpens > 0 {
		report.Rates.CTR = float64(m.UniqueClicks) / float64(m.UniqueOpens) * 100
	}

	t.mu.Unlock()
	return report
}

// FlushToS3 writes a campaign report to S3.
func (t *CampaignEventTracker) FlushToS3(ctx context.Context, campaignID string) error {
	report := t.GetReport(campaignID)
	if report == nil {
		return fmt.Errorf("campaign %s not found", campaignID)
	}

	if t.s3Client == nil {
		log.Printf("[campaign-events] S3 not configured, skipping flush for %s", campaignID)
		return nil
	}

	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}

	key := fmt.Sprintf("campaigns/%s/report_%s.json", campaignID, time.Now().Format("2006-01-02T15-04-05"))
	_, err = t.s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(t.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(data),
		ContentType: aws.String("application/json"),
	})
	if err != nil {
		return fmt.Errorf("s3 put %s: %w", key, err)
	}

	// Also write a latest report
	latestKey := fmt.Sprintf("campaigns/%s/latest.json", campaignID)
	_, err = t.s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(t.bucket),
		Key:         aws.String(latestKey),
		Body:        bytes.NewReader(data),
		ContentType: aws.String("application/json"),
	})
	if err != nil {
		log.Printf("[campaign-events] write latest error: %v", err)
	}

	log.Printf("[campaign-events] flushed report for %s to s3://%s/%s", campaignID, t.bucket, key)
	return nil
}

// FlushAll writes all active campaign reports to S3.
func (t *CampaignEventTracker) FlushAll(ctx context.Context) {
	t.mu.Lock()
	ids := make([]string, 0, len(t.campaigns))
	for id := range t.campaigns {
		ids = append(ids, id)
	}
	t.mu.Unlock()

	for _, id := range ids {
		if err := t.FlushToS3(ctx, id); err != nil {
			log.Printf("[campaign-events] flush error campaign=%s: %v", id, err)
		}
	}
}

// ClassifyBounce categorizes a PMTA bounce category into soft or hard bounce.
func ClassifyBounce(bounceCat string) string {
	switch bounceCat {
	case "bad-mailbox", "bad-domain", "no-answer-from-host",
		"inactive-mailbox", "policy-related", "routing-errors",
		"bad-connection":
		return "hard_bounce"
	case "quota-issues", "message-expired", "rate-limited",
		"content-related", "other":
		return "soft_bounce"
	default:
		return "soft_bounce"
	}
}
