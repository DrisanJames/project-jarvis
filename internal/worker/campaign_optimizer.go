package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/mailing"
)

// CampaignOptimizer monitors active campaigns and makes AI-driven optimization decisions
// based on the campaign's purpose (data_activation vs offer_revenue)
type CampaignOptimizer struct {
	mu              sync.RWMutex
	db              *sql.DB
	objectivesStore *mailing.ObjectivesStore
	running         bool
	stopCh          chan struct{}

	// Active campaign trackers
	activeCampaigns map[uuid.UUID]*CampaignTracker

	// Configuration
	pollInterval   time.Duration
	signalInterval time.Duration
}

// CampaignTracker tracks real-time performance for an active campaign
type CampaignTracker struct {
	CampaignID uuid.UUID
	Objective  *mailing.CampaignObjective
	StartedAt  time.Time

	// Real-time metrics
	CurrentThrottle int
	SentCount       int
	DeliveredCount  int
	OpenCount       int
	ClickCount      int
	BounceCount     int
	ComplaintCount  int
	ConversionCount int
	Revenue         float64

	// ISP breakdown
	ISPMetrics map[string]*ISPMetrics

	// Signal history (last N signals)
	RecentSignals []mailing.ESPSignal

	// Last optimization time
	LastOptimization time.Time
}

// ISPMetrics tracks per-ISP performance
type ISPMetrics struct {
	ISP            string
	Sent           int
	Delivered      int
	Bounced        int
	Complaints     int
	DeliveryRate   float64
	IsHealthy      bool
	LastSignalTime time.Time
}

// OptimizationDecision represents an AI decision
type OptimizationDecision struct {
	Type       string  // throughput_increase, throughput_decrease, pause, rotate_creative, etc.
	Reason     string
	OldValue   string
	NewValue   string
	Confidence float64
}

// NewCampaignOptimizer creates a new campaign optimizer
func NewCampaignOptimizer(db *sql.DB, objectivesStore *mailing.ObjectivesStore) *CampaignOptimizer {
	return &CampaignOptimizer{
		db:              db,
		objectivesStore: objectivesStore,
		stopCh:          make(chan struct{}),
		activeCampaigns: make(map[uuid.UUID]*CampaignTracker),
		pollInterval:    30 * time.Second,
		signalInterval:  1 * time.Minute,
	}
}

// Start begins the optimization loop
func (co *CampaignOptimizer) Start() {
	co.mu.Lock()
	if co.running {
		co.mu.Unlock()
		return
	}
	co.running = true
	co.mu.Unlock()

	log.Println("[CampaignOptimizer] Starting optimization loop")

	go co.optimizationLoop()
}

// Stop stops the optimization loop
func (co *CampaignOptimizer) Stop() {
	co.mu.Lock()
	if !co.running {
		co.mu.Unlock()
		return
	}
	co.running = false
	co.mu.Unlock()

	close(co.stopCh)
	log.Println("[CampaignOptimizer] Stopped")
}

// optimizationLoop runs continuously to monitor and optimize active campaigns
func (co *CampaignOptimizer) optimizationLoop() {
	ticker := time.NewTicker(co.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			co.processActiveCampaigns()
		case <-co.stopCh:
			return
		}
	}
}

// processActiveCampaigns processes all active sending campaigns
func (co *CampaignOptimizer) processActiveCampaigns() {
	ctx := context.Background()

	// Get active campaigns (status = 'sending')
	rows, err := co.db.QueryContext(ctx, `
		SELECT c.id, c.name, c.sent_count, c.delivered_count, 
		       c.open_count, c.click_count, c.bounce_count, c.complaint_count,
		       c.revenue
		FROM mailing_campaigns c
		WHERE c.status = 'sending'
	`)
	if err != nil {
		log.Printf("[CampaignOptimizer] Error fetching active campaigns: %v", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var campaignID uuid.UUID
		var name string
		var sent, delivered, opens, clicks, bounces, complaints int
		var revenue float64

		if err := rows.Scan(&campaignID, &name, &sent, &delivered, &opens, &clicks, &bounces, &complaints, &revenue); err != nil {
			continue
		}

		// Get or create tracker
		tracker := co.getOrCreateTracker(ctx, campaignID)
		if tracker == nil || tracker.Objective == nil {
			continue
		}

		// Update metrics
		tracker.SentCount = sent
		tracker.DeliveredCount = delivered
		tracker.OpenCount = opens
		tracker.ClickCount = clicks
		tracker.BounceCount = bounces
		tracker.ComplaintCount = complaints
		tracker.Revenue = revenue

		// Analyze and optimize based on purpose
		co.analyzeAndOptimize(ctx, tracker)
	}
}

// getOrCreateTracker gets or creates a campaign tracker
func (co *CampaignOptimizer) getOrCreateTracker(ctx context.Context, campaignID uuid.UUID) *CampaignTracker {
	co.mu.RLock()
	tracker, exists := co.activeCampaigns[campaignID]
	co.mu.RUnlock()

	if exists {
		return tracker
	}

	// Load objective
	objective, err := co.objectivesStore.GetObjective(ctx, campaignID)
	if err != nil {
		// No objective configured, use default behavior
		return nil
	}

	tracker = &CampaignTracker{
		CampaignID:       campaignID,
		Objective:        objective,
		StartedAt:        time.Now(),
		ISPMetrics:       make(map[string]*ISPMetrics),
		RecentSignals:    make([]mailing.ESPSignal, 0),
		LastOptimization: time.Now(),
	}

	co.mu.Lock()
	co.activeCampaigns[campaignID] = tracker
	co.mu.Unlock()

	log.Printf("[CampaignOptimizer] Started tracking campaign %s (purpose: %s)", campaignID, objective.Purpose)

	return tracker
}

// analyzeAndOptimize analyzes campaign performance and makes optimization decisions
func (co *CampaignOptimizer) analyzeAndOptimize(ctx context.Context, tracker *CampaignTracker) {
	obj := tracker.Objective

	// Don't optimize too frequently
	if time.Since(tracker.LastOptimization) < time.Minute {
		return
	}

	decisions := make([]OptimizationDecision, 0)

	// Load recent signals
	signals, _ := co.objectivesStore.GetRecentSignals(ctx, tracker.CampaignID, time.Now().Add(-15*time.Minute))
	tracker.RecentSignals = signals

	// Calculate current rates
	complaintRate := float64(0)
	if tracker.DeliveredCount > 0 {
		complaintRate = float64(tracker.ComplaintCount) / float64(tracker.DeliveredCount)
	}

	bounceRate := float64(0)
	if tracker.SentCount > 0 {
		bounceRate = float64(tracker.BounceCount) / float64(tracker.SentCount)
	}

	// Check for spam signals - applicable to both purposes
	if obj.PauseOnSpamSignal {
		threshold := obj.SpamSignalThreshold
		if threshold == nil {
			defaultThreshold := 0.001
			threshold = &defaultThreshold
		}
		if complaintRate > *threshold {
			decisions = append(decisions, OptimizationDecision{
				Type:       "pause_campaign",
				Reason:     fmt.Sprintf("Complaint rate %.4f%% exceeds threshold %.4f%%", complaintRate*100, *threshold*100),
				Confidence: 0.95,
			})
		}
	}

	// Check bounce threshold
	if obj.BounceThreshold != nil && bounceRate > *obj.BounceThreshold {
		decisions = append(decisions, OptimizationDecision{
			Type:       "throughput_decrease",
			Reason:     fmt.Sprintf("Bounce rate %.2f%% exceeds threshold %.2f%%", bounceRate*100, *obj.BounceThreshold*100),
			OldValue:   fmt.Sprintf("%d", tracker.CurrentThrottle),
			NewValue:   fmt.Sprintf("%d", int(float64(tracker.CurrentThrottle)*0.5)),
			Confidence: 0.85,
		})
	}

	// Purpose-specific optimization
	switch obj.Purpose {
	case "data_activation":
		decisions = append(decisions, co.optimizeDataActivation(ctx, tracker)...)
	case "offer_revenue":
		decisions = append(decisions, co.optimizeOfferRevenue(ctx, tracker)...)
	}

	// Execute decisions
	for _, decision := range decisions {
		co.executeDecision(ctx, tracker, decision)
	}

	tracker.LastOptimization = time.Now()
}

// optimizeDataActivation optimizes for data activation campaigns
func (co *CampaignOptimizer) optimizeDataActivation(ctx context.Context, tracker *CampaignTracker) []OptimizationDecision {
	decisions := make([]OptimizationDecision, 0)
	obj := tracker.Objective

	// Calculate engagement rate
	engagementRate := float64(0)
	if tracker.DeliveredCount > 0 {
		engagementRate = float64(tracker.OpenCount) / float64(tracker.DeliveredCount)
	}

	// Check if meeting engagement target
	if obj.TargetEngagementRate != nil && engagementRate < *obj.TargetEngagementRate {
		// Engagement below target - consider rotating creative or slowing down
		if obj.AICreativeRotation {
			decisions = append(decisions, OptimizationDecision{
				Type:       "rotate_creative",
				Reason:     fmt.Sprintf("Engagement rate %.2f%% below target %.2f%%", engagementRate*100, *obj.TargetEngagementRate*100),
				Confidence: 0.70,
			})
		}
	}

	// Analyze ISP signals for data activation
	spamSignals := 0
	deliveredSignals := 0
	for _, signal := range tracker.RecentSignals {
		switch signal.SignalType {
		case "spam_complaint", "policy_rejection", "blocked":
			spamSignals += signal.SignalCount
		case "delivered":
			deliveredSignals += signal.SignalCount
		}
	}

	// If ISPs are accepting well, can increase throughput
	if deliveredSignals > 100 && spamSignals == 0 && obj.AIThroughputOptimization {
		// Good signals, consider ramping up
		sensitivity := co.getSensitivityMultiplier(obj.ThroughputSensitivity)
		newRate := int(float64(tracker.CurrentThrottle) * (1 + 0.1*sensitivity))
		if newRate > obj.MaxThroughputRate {
			newRate = obj.MaxThroughputRate
		}

		if newRate > tracker.CurrentThrottle {
			decisions = append(decisions, OptimizationDecision{
				Type:       "throughput_increase",
				Reason:     "ISPs accepting mail well, no spam signals detected",
				OldValue:   fmt.Sprintf("%d", tracker.CurrentThrottle),
				NewValue:   fmt.Sprintf("%d", newRate),
				Confidence: 0.80,
			})
		}
	}

	return decisions
}

// optimizeOfferRevenue optimizes for offer revenue campaigns
func (co *CampaignOptimizer) optimizeOfferRevenue(ctx context.Context, tracker *CampaignTracker) []OptimizationDecision {
	decisions := make([]OptimizationDecision, 0)
	obj := tracker.Objective

	// Calculate current eCPM
	currentECPM := float64(0)
	if tracker.SentCount > 0 {
		currentECPM = (tracker.Revenue / float64(tracker.SentCount)) * 1000
	}

	// Check budget utilization
	if obj.BudgetLimit != nil && *obj.BudgetLimit > 0 {
		budgetUsed := tracker.Revenue
		if budgetUsed >= *obj.BudgetLimit {
			decisions = append(decisions, OptimizationDecision{
				Type:       "pause_campaign",
				Reason:     fmt.Sprintf("Budget exhausted: $%.2f spent of $%.2f limit", budgetUsed, *obj.BudgetLimit),
				Confidence: 1.0,
			})
			return decisions
		}

		// Budget pacing - are we spending too fast or too slow?
		if obj.AIBudgetPacing && obj.TargetCompletionHours > 0 {
			elapsed := time.Since(tracker.StartedAt).Hours()
			if elapsed > 0 {
				expectedSpend := (*obj.BudgetLimit / float64(obj.TargetCompletionHours)) * elapsed
				actualSpend := budgetUsed

				if actualSpend > expectedSpend*1.2 {
					// Spending too fast, reduce throughput
					decisions = append(decisions, OptimizationDecision{
						Type:       "throughput_decrease",
						Reason:     fmt.Sprintf("Overspending: $%.2f vs expected $%.2f", actualSpend, expectedSpend),
						Confidence: 0.85,
					})
				} else if actualSpend < expectedSpend*0.8 {
					// Spending too slow, increase throughput
					decisions = append(decisions, OptimizationDecision{
						Type:       "throughput_increase",
						Reason:     fmt.Sprintf("Underspending: $%.2f vs expected $%.2f", actualSpend, expectedSpend),
						Confidence: 0.75,
					})
				}
			}
		}
	}

	// Check eCPM target
	if obj.ECPMTarget != nil && *obj.ECPMTarget > 0 && tracker.SentCount > 1000 {
		if currentECPM < *obj.ECPMTarget*0.8 {
			// eCPM below target, try rotating creative
			if obj.AICreativeRotation {
				decisions = append(decisions, OptimizationDecision{
					Type:       "rotate_creative",
					Reason:     fmt.Sprintf("eCPM $%.2f below target $%.2f", currentECPM, *obj.ECPMTarget),
					Confidence: 0.70,
				})
			}
		} else if currentECPM > *obj.ECPMTarget*1.2 {
			// eCPM exceeding target, can increase throughput
			if obj.AIThroughputOptimization {
				decisions = append(decisions, OptimizationDecision{
					Type:       "throughput_increase",
					Reason:     fmt.Sprintf("eCPM $%.2f exceeding target $%.2f", currentECPM, *obj.ECPMTarget),
					Confidence: 0.80,
				})
			}
		}
	}

	// Check target achievement
	if obj.TargetMetric != "" && obj.TargetValue > 0 {
		achieved := 0
		switch obj.TargetMetric {
		case "clicks":
			achieved = tracker.ClickCount
		case "conversions":
			achieved = tracker.ConversionCount
		}

		if achieved >= obj.TargetValue {
			decisions = append(decisions, OptimizationDecision{
				Type:       "target_achieved",
				Reason:     fmt.Sprintf("Target achieved: %d %s of %d target", achieved, obj.TargetMetric, obj.TargetValue),
				Confidence: 1.0,
			})
		}
	}

	return decisions
}

// executeDecision executes an optimization decision
func (co *CampaignOptimizer) executeDecision(ctx context.Context, tracker *CampaignTracker, decision OptimizationDecision) {
	log.Printf("[CampaignOptimizer] Campaign %s: %s - %s (confidence: %.2f)",
		tracker.CampaignID, decision.Type, decision.Reason, decision.Confidence)

	// Log the decision
	logEntry := &mailing.CampaignOptimizationLog{
		CampaignID:       tracker.CampaignID,
		OrganizationID:   tracker.Objective.OrganizationID,
		OptimizationType: decision.Type,
		TriggerReason:    decision.Reason,
		TriggerMetrics:   co.getMetricsSnapshot(tracker),
		OldValue:         decision.OldValue,
		NewValue:         decision.NewValue,
		AIReasoning:      decision.Reason,
		AIConfidence:     &decision.Confidence,
		Applied:          true,
	}
	now := time.Now()
	logEntry.AppliedAt = &now

	co.objectivesStore.LogOptimization(ctx, logEntry)

	// Execute the actual decision
	switch decision.Type {
	case "pause_campaign":
		co.pauseCampaign(ctx, tracker.CampaignID)
	case "throughput_increase", "throughput_decrease":
		co.adjustThrottle(ctx, tracker, decision)
	case "rotate_creative":
		co.rotateCreative(ctx, tracker)
	case "target_achieved":
		log.Printf("[CampaignOptimizer] Campaign %s achieved target", tracker.CampaignID)
	}
}

// pauseCampaign pauses a campaign
func (co *CampaignOptimizer) pauseCampaign(ctx context.Context, campaignID uuid.UUID) {
	_, err := co.db.ExecContext(ctx,
		"UPDATE mailing_campaigns SET status = 'paused', updated_at = NOW() WHERE id = $1",
		campaignID,
	)
	if err != nil {
		log.Printf("[CampaignOptimizer] Error pausing campaign %s: %v", campaignID, err)
	}
}

// adjustThrottle adjusts the campaign throttle rate
func (co *CampaignOptimizer) adjustThrottle(ctx context.Context, tracker *CampaignTracker, decision OptimizationDecision) {
	// Update the throttle in mailing_campaign_ai_settings
	_, err := co.db.ExecContext(ctx, `
		UPDATE mailing_campaign_ai_settings 
		SET current_throttle_rate = $2, updated_at = NOW()
		WHERE campaign_id = $1
	`, tracker.CampaignID, decision.NewValue)
	if err != nil {
		log.Printf("[CampaignOptimizer] Error adjusting throttle for %s: %v", tracker.CampaignID, err)
	}
}

// rotateCreative rotates to the next creative
func (co *CampaignOptimizer) rotateCreative(ctx context.Context, tracker *CampaignTracker) {
	err := co.objectivesStore.IncrementCreativeIndex(ctx, tracker.CampaignID)
	if err != nil {
		log.Printf("[CampaignOptimizer] Error rotating creative for %s: %v", tracker.CampaignID, err)
	}
}

// getSensitivityMultiplier returns a multiplier based on throughput sensitivity
func (co *CampaignOptimizer) getSensitivityMultiplier(sensitivity string) float64 {
	switch sensitivity {
	case "low":
		return 0.5
	case "high":
		return 2.0
	default:
		return 1.0
	}
}

// getMetricsSnapshot returns a JSON snapshot of current metrics
func (co *CampaignOptimizer) getMetricsSnapshot(tracker *CampaignTracker) []byte {
	snapshot := map[string]interface{}{
		"sent_count":      tracker.SentCount,
		"delivered_count": tracker.DeliveredCount,
		"open_count":      tracker.OpenCount,
		"click_count":     tracker.ClickCount,
		"bounce_count":    tracker.BounceCount,
		"complaint_count": tracker.ComplaintCount,
		"revenue":         tracker.Revenue,
		"timestamp":       time.Now().Format(time.RFC3339),
	}
	data, _ := json.Marshal(snapshot)
	return data
}

// RemoveTracker removes a campaign from active tracking
func (co *CampaignOptimizer) RemoveTracker(campaignID uuid.UUID) {
	co.mu.Lock()
	delete(co.activeCampaigns, campaignID)
	co.mu.Unlock()
}

// GetActiveTrackers returns the number of active campaign trackers
func (co *CampaignOptimizer) GetActiveTrackers() int {
	co.mu.RLock()
	defer co.mu.RUnlock()
	return len(co.activeCampaigns)
}

// GetTrackerStatus returns status info for a specific campaign tracker
func (co *CampaignOptimizer) GetTrackerStatus(campaignID uuid.UUID) map[string]interface{} {
	co.mu.RLock()
	tracker, exists := co.activeCampaigns[campaignID]
	co.mu.RUnlock()

	if !exists {
		return nil
	}

	return map[string]interface{}{
		"campaign_id":       tracker.CampaignID,
		"purpose":           tracker.Objective.Purpose,
		"started_at":        tracker.StartedAt,
		"sent_count":        tracker.SentCount,
		"delivered_count":   tracker.DeliveredCount,
		"open_count":        tracker.OpenCount,
		"click_count":       tracker.ClickCount,
		"bounce_count":      tracker.BounceCount,
		"complaint_count":   tracker.ComplaintCount,
		"revenue":           tracker.Revenue,
		"current_throttle":  tracker.CurrentThrottle,
		"last_optimization": tracker.LastOptimization,
		"recent_signals":    len(tracker.RecentSignals),
	}
}

// IsRunning returns whether the optimizer is currently running
func (co *CampaignOptimizer) IsRunning() bool {
	co.mu.RLock()
	defer co.mu.RUnlock()
	return co.running
}
