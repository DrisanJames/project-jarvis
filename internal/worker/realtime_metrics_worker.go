package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// ============================================================================
// REAL-TIME METRICS WORKER
// ============================================================================
// Collects and aggregates campaign metrics in real-time
// - Polls active campaigns every 30 seconds
// - Updates mailing_campaign_realtime_metrics
// - Triggers AI optimization every 5 minutes

// RealtimeMetricsWorker handles real-time metrics collection
type RealtimeMetricsWorker struct {
	db    *sql.DB
	redis *redis.Client

	// Configuration
	pollInterval       time.Duration
	optimizationInterval time.Duration
	
	// State
	running    bool
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	
	// Optimization tracking (campaign_id -> last optimization time)
	lastOptimization   map[string]time.Time
	lastOptimizationMu sync.Mutex
}

// NewRealtimeMetricsWorker creates a new real-time metrics worker
func NewRealtimeMetricsWorker(db *sql.DB, redisClient *redis.Client) *RealtimeMetricsWorker {
	return &RealtimeMetricsWorker{
		db:                   db,
		redis:                redisClient,
		pollInterval:         30 * time.Second,
		optimizationInterval: 5 * time.Minute,
		lastOptimization:     make(map[string]time.Time),
	}
}

// Start begins the metrics collection loop
func (w *RealtimeMetricsWorker) Start() {
	if w.running {
		return
	}
	
	w.running = true
	w.ctx, w.cancel = context.WithCancel(context.Background())
	
	log.Printf("[RealtimeMetricsWorker] Starting with poll interval %v", w.pollInterval)
	
	w.wg.Add(1)
	go w.runLoop()
}

// Stop gracefully stops the worker
func (w *RealtimeMetricsWorker) Stop() {
	if !w.running {
		return
	}
	
	log.Println("[RealtimeMetricsWorker] Stopping...")
	w.cancel()
	w.wg.Wait()
	w.running = false
	log.Println("[RealtimeMetricsWorker] Stopped")
}

func (w *RealtimeMetricsWorker) runLoop() {
	defer w.wg.Done()
	
	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()
	
	for {
		select {
		case <-w.ctx.Done():
			return
		case <-ticker.C:
			w.collectMetrics()
		}
	}
}

func (w *RealtimeMetricsWorker) collectMetrics() {
	ctx, cancel := context.WithTimeout(w.ctx, 25*time.Second)
	defer cancel()
	
	// Get all active campaigns (status = 'sending')
	campaigns, err := w.getActiveCampaigns(ctx)
	if err != nil {
		log.Printf("[RealtimeMetricsWorker] Error getting active campaigns: %v", err)
		return
	}
	
	if len(campaigns) == 0 {
		return
	}
	
	log.Printf("[RealtimeMetricsWorker] Processing %d active campaigns", len(campaigns))
	
	for _, campaignID := range campaigns {
		// Record metrics
		if err := w.recordCampaignMetrics(ctx, campaignID); err != nil {
			log.Printf("[RealtimeMetricsWorker] Error recording metrics for %s: %v", campaignID, err)
			continue
		}
		
		// Check if AI optimization is due
		w.maybeRunOptimization(ctx, campaignID)
	}
}

func (w *RealtimeMetricsWorker) getActiveCampaigns(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := w.db.QueryContext(ctx, `
		SELECT id FROM mailing_campaigns
		WHERE status = 'sending'
		  AND started_at IS NOT NULL
		  AND completed_at IS NULL
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	
	var campaigns []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			continue
		}
		campaigns = append(campaigns, id)
	}
	
	return campaigns, nil
}

func (w *RealtimeMetricsWorker) recordCampaignMetrics(ctx context.Context, campaignID uuid.UUID) error {
	// Get current campaign stats
	var sentCount, deliveredCount, openCount, uniqueOpenCount int
	var clickCount, uniqueClickCount, bounceCount, complaintCount, unsubscribeCount int
	
	err := w.db.QueryRowContext(ctx, `
		SELECT COALESCE(sent_count, 0), COALESCE(delivered_count, 0), 
		       COALESCE(open_count, 0), COALESCE(unique_open_count, 0),
		       COALESCE(click_count, 0), COALESCE(unique_click_count, 0),
		       COALESCE(bounce_count, 0), COALESCE(complaint_count, 0), 
		       COALESCE(unsubscribe_count, 0)
		FROM mailing_campaigns
		WHERE id = $1
	`, campaignID).Scan(
		&sentCount, &deliveredCount, &openCount, &uniqueOpenCount,
		&clickCount, &uniqueClickCount, &bounceCount, &complaintCount, &unsubscribeCount,
	)
	if err != nil {
		return fmt.Errorf("get campaign stats: %w", err)
	}
	
	// Get previous cumulative totals
	var prevSent, prevOpens, prevClicks, prevBounces, prevComplaints int
	err = w.db.QueryRowContext(ctx, `
		SELECT COALESCE(cumulative_sent, 0), COALESCE(cumulative_opens, 0),
		       COALESCE(cumulative_clicks, 0), COALESCE(cumulative_bounces, 0),
		       COALESCE(cumulative_complaints, 0)
		FROM mailing_campaign_realtime_metrics
		WHERE campaign_id = $1
		ORDER BY timestamp DESC
		LIMIT 1
	`, campaignID).Scan(&prevSent, &prevOpens, &prevClicks, &prevBounces, &prevComplaints)
	if err == sql.ErrNoRows {
		// First record
		prevSent, prevOpens, prevClicks, prevBounces, prevComplaints = 0, 0, 0, 0, 0
	} else if err != nil {
		return fmt.Errorf("get previous metrics: %w", err)
	}
	
	// Get current throttle rate from AI settings
	var currentThrottleRate int = 10000
	err = w.db.QueryRowContext(ctx, `
		SELECT COALESCE(current_throttle_rate, 10000)
		FROM mailing_campaign_ai_settings
		WHERE campaign_id = $1
	`, campaignID).Scan(&currentThrottleRate)
	if err != nil && err != sql.ErrNoRows {
		log.Printf("[RealtimeMetricsWorker] Error getting throttle rate: %v", err)
	}
	
	// Calculate rates
	var openRate, clickRate, bounceRate, complaintRate float64
	if sentCount > 0 {
		openRate = float64(openCount) / float64(sentCount)
		clickRate = float64(clickCount) / float64(sentCount)
		bounceRate = float64(bounceCount) / float64(sentCount)
		complaintRate = float64(complaintCount) / float64(sentCount)
	}
	
	// Calculate throttle utilization
	intervalSent := sentCount - prevSent
	var throttleUtilization float64
	if currentThrottleRate > 0 {
		// Convert to per-30-seconds
		throttlePer30Sec := currentThrottleRate / 120
		if throttlePer30Sec > 0 {
			throttleUtilization = float64(intervalSent) / float64(throttlePer30Sec)
		}
	}
	
	now := time.Now()
	intervalStart := now.Add(-30 * time.Second)
	
	// Insert metrics record
	_, err = w.db.ExecContext(ctx, `
		INSERT INTO mailing_campaign_realtime_metrics (
			campaign_id, timestamp, interval_start, interval_end,
			sent_count, delivered_count, open_count, unique_open_count,
			click_count, unique_click_count, bounce_count,
			complaint_count, unsubscribe_count,
			open_rate, click_rate, bounce_rate, complaint_rate,
			cumulative_sent, cumulative_opens, cumulative_clicks,
			cumulative_bounces, cumulative_complaints,
			current_throttle_rate, throttle_utilization
		) VALUES (
			$1, $2, $3, $4,
			$5, $6, $7, $8,
			$9, $10, $11,
			$12, $13,
			$14, $15, $16, $17,
			$18, $19, $20,
			$21, $22,
			$23, $24
		)
	`, campaignID, now, intervalStart, now,
		intervalSent, deliveredCount, openCount-prevOpens, uniqueOpenCount,
		clickCount-prevClicks, uniqueClickCount, bounceCount-prevBounces,
		complaintCount-prevComplaints, unsubscribeCount,
		openRate, clickRate, bounceRate, complaintRate,
		sentCount, openCount, clickCount,
		bounceCount, complaintCount,
		currentThrottleRate, throttleUtilization,
	)
	
	if err != nil {
		return fmt.Errorf("insert metrics: %w", err)
	}
	
	// Also update Redis for real-time dashboard access
	w.updateRedisMetrics(ctx, campaignID, sentCount, openCount, clickCount, bounceCount, complaintCount)
	
	return nil
}

func (w *RealtimeMetricsWorker) updateRedisMetrics(ctx context.Context, campaignID uuid.UUID, sent, opens, clicks, bounces, complaints int) {
	if w.redis == nil {
		return
	}
	
	key := fmt.Sprintf("campaign:%s:realtime", campaignID.String())
	metrics := map[string]interface{}{
		"sent":       sent,
		"opens":      opens,
		"clicks":     clicks,
		"bounces":    bounces,
		"complaints": complaints,
		"updated_at": time.Now().Unix(),
	}
	
	data, _ := json.Marshal(metrics)
	w.redis.Set(ctx, key, data, 5*time.Minute)
}

func (w *RealtimeMetricsWorker) maybeRunOptimization(ctx context.Context, campaignID uuid.UUID) {
	w.lastOptimizationMu.Lock()
	lastRun, exists := w.lastOptimization[campaignID.String()]
	w.lastOptimizationMu.Unlock()
	
	if exists && time.Since(lastRun) < w.optimizationInterval {
		return
	}
	
	// Check if AI optimization is enabled
	var enabled bool
	err := w.db.QueryRowContext(ctx, `
		SELECT enable_throttle_optimization
		FROM mailing_campaign_ai_settings
		WHERE campaign_id = $1
	`, campaignID).Scan(&enabled)
	
	if err != nil || !enabled {
		return
	}
	
	// Run optimization
	log.Printf("[RealtimeMetricsWorker] Running AI optimization for campaign %s", campaignID)
	
	result, err := w.runThrottleOptimization(ctx, campaignID)
	if err != nil {
		log.Printf("[RealtimeMetricsWorker] Optimization error for %s: %v", campaignID, err)
		return
	}
	
	if result.NewRate != result.PreviousRate {
		log.Printf("[RealtimeMetricsWorker] Throttle adjusted for %s: %d -> %d (%s)",
			campaignID, result.PreviousRate, result.NewRate, result.Reason)
	}
	
	w.lastOptimizationMu.Lock()
	w.lastOptimization[campaignID.String()] = time.Now()
	w.lastOptimizationMu.Unlock()
}

// ThrottleOptimizationResult for the worker
type ThrottleOptimizationResult struct {
	PreviousRate int
	NewRate      int
	Reason       string
}

func (w *RealtimeMetricsWorker) runThrottleOptimization(ctx context.Context, campaignID uuid.UUID) (*ThrottleOptimizationResult, error) {
	// Get current settings
	var currentRate, minRate, maxRate int
	var complaintThreshold, bounceThreshold float64
	
	err := w.db.QueryRowContext(ctx, `
		SELECT current_throttle_rate, min_throttle_rate, max_throttle_rate,
		       complaint_threshold, bounce_threshold
		FROM mailing_campaign_ai_settings
		WHERE campaign_id = $1
	`, campaignID).Scan(&currentRate, &minRate, &maxRate, &complaintThreshold, &bounceThreshold)
	
	if err == sql.ErrNoRows {
		currentRate, minRate, maxRate = 10000, 1000, 50000
		complaintThreshold, bounceThreshold = 0.001, 0.05
	} else if err != nil {
		return nil, err
	}
	
	// Get recent metrics (last 15 minutes)
	rows, err := w.db.QueryContext(ctx, `
		SELECT cumulative_sent, cumulative_bounces, cumulative_complaints, cumulative_opens
		FROM mailing_campaign_realtime_metrics
		WHERE campaign_id = $1 AND timestamp >= NOW() - INTERVAL '15 minutes'
		ORDER BY timestamp DESC
		LIMIT 30
	`, campaignID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	
	var totalSent, totalBounces, totalComplaints, totalOpens int
	count := 0
	for rows.Next() {
		var sent, bounces, complaints, opens int
		rows.Scan(&sent, &bounces, &complaints, &opens)
		if count == 0 {
			// Use most recent cumulative values
			totalSent = sent
			totalBounces = bounces
			totalComplaints = complaints
			totalOpens = opens
		}
		count++
	}
	
	if totalSent == 0 {
		return &ThrottleOptimizationResult{
			PreviousRate: currentRate,
			NewRate:      currentRate,
			Reason:       "No data for optimization",
		}, nil
	}
	
	bounceRate := float64(totalBounces) / float64(totalSent)
	complaintRate := float64(totalComplaints) / float64(totalSent)
	openRate := float64(totalOpens) / float64(totalSent)
	
	result := &ThrottleOptimizationResult{PreviousRate: currentRate}
	
	// Decision logic
	if complaintRate > complaintThreshold*2 {
		// Critical - pause
		result.NewRate = 0
		result.Reason = fmt.Sprintf("Critical complaint rate %.4f%% - pausing", complaintRate*100)
		w.pauseCampaign(ctx, campaignID, result.Reason)
	} else if complaintRate > complaintThreshold {
		// High complaints - reduce 50%
		result.NewRate = int(float64(currentRate) * 0.5)
		result.Reason = fmt.Sprintf("High complaint rate %.4f%% - reducing 50%%", complaintRate*100)
	} else if bounceRate > bounceThreshold {
		// High bounces - reduce 30%
		result.NewRate = int(float64(currentRate) * 0.7)
		result.Reason = fmt.Sprintf("High bounce rate %.2f%% - reducing 30%%", bounceRate*100)
	} else if complaintRate < complaintThreshold*0.5 && bounceRate < bounceThreshold*0.5 && openRate > 0.10 {
		// Good metrics - increase 25%
		result.NewRate = int(float64(currentRate) * 1.25)
		result.Reason = fmt.Sprintf("Strong metrics (%.2f%% open) - increasing 25%%", openRate*100)
	} else {
		// Maintain
		result.NewRate = currentRate
		result.Reason = "Metrics acceptable - maintaining rate"
	}
	
	// Apply limits
	if result.NewRate > 0 {
		result.NewRate = max(result.NewRate, minRate)
		result.NewRate = min(result.NewRate, maxRate)
	}
	
	// Update rate if changed
	if result.NewRate != currentRate && result.NewRate > 0 {
		_, err = w.db.ExecContext(ctx, `
			UPDATE mailing_campaign_ai_settings
			SET current_throttle_rate = $2, updated_at = NOW()
			WHERE campaign_id = $1
		`, campaignID, result.NewRate)
		if err != nil {
			log.Printf("[RealtimeMetricsWorker] Failed to update throttle: %v", err)
		}
		
		// Log decision
		w.logAIDecision(ctx, campaignID, result)
	}
	
	return result, nil
}

func (w *RealtimeMetricsWorker) pauseCampaign(ctx context.Context, campaignID uuid.UUID, reason string) {
	_, err := w.db.ExecContext(ctx, `
		UPDATE mailing_campaigns
		SET status = 'paused', updated_at = NOW()
		WHERE id = $1
	`, campaignID)
	
	if err != nil {
		log.Printf("[RealtimeMetricsWorker] Failed to pause campaign: %v", err)
		return
	}
	
	// Create alert
	_, err = w.db.ExecContext(ctx, `
		INSERT INTO mailing_campaign_alerts (
			campaign_id, alert_type, severity, title, message, auto_action_taken
		) VALUES ($1, 'high_complaint', 'critical', 'Campaign Auto-Paused', $2, 'paused')
	`, campaignID, reason)
	
	if err != nil {
		log.Printf("[RealtimeMetricsWorker] Failed to create alert: %v", err)
	}
}

func (w *RealtimeMetricsWorker) logAIDecision(ctx context.Context, campaignID uuid.UUID, result *ThrottleOptimizationResult) {
	decisionType := "throttle_adjust"
	if result.NewRate > result.PreviousRate {
		decisionType = "throttle_increase"
	} else if result.NewRate < result.PreviousRate {
		decisionType = "throttle_decrease"
	}
	
	_, err := w.db.ExecContext(ctx, `
		INSERT INTO mailing_ai_decisions (
			campaign_id, decision_type, decision_reason, old_value, new_value,
			metrics_snapshot, ai_model, confidence, applied, applied_at
		) VALUES ($1, $2, $3, $4, $5, '{}', 'rules-based', 0.8, true, NOW())
	`, campaignID, decisionType, result.Reason,
		fmt.Sprintf("%d", result.PreviousRate), fmt.Sprintf("%d", result.NewRate))
	
	if err != nil {
		log.Printf("[RealtimeMetricsWorker] Failed to log AI decision: %v", err)
	}
}

// ============================================================================
// A/B TEST WINNER SELECTION WORKER
// ============================================================================

// ABTestWorker handles A/B test winner selection
type ABTestWorker struct {
	db    *sql.DB
	redis *redis.Client
	
	// Configuration
	checkInterval time.Duration
	
	// State
	running bool
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// NewABTestWorker creates a new A/B test worker
func NewABTestWorker(db *sql.DB, redisClient *redis.Client) *ABTestWorker {
	return &ABTestWorker{
		db:            db,
		redis:         redisClient,
		checkInterval: 1 * time.Minute,
	}
}

// Start begins the A/B test worker
func (w *ABTestWorker) Start() {
	if w.running {
		return
	}
	
	w.running = true
	w.ctx, w.cancel = context.WithCancel(context.Background())
	
	log.Println("[ABTestWorker] Starting")
	
	w.wg.Add(1)
	go w.runLoop()
}

// Stop gracefully stops the worker
func (w *ABTestWorker) Stop() {
	if !w.running {
		return
	}
	
	log.Println("[ABTestWorker] Stopping...")
	w.cancel()
	w.wg.Wait()
	w.running = false
	log.Println("[ABTestWorker] Stopped")
}

func (w *ABTestWorker) runLoop() {
	defer w.wg.Done()
	
	ticker := time.NewTicker(w.checkInterval)
	defer ticker.Stop()
	
	for {
		select {
		case <-w.ctx.Done():
			return
		case <-ticker.C:
			w.checkABTests()
		}
	}
}

func (w *ABTestWorker) checkABTests() {
	ctx, cancel := context.WithTimeout(w.ctx, 30*time.Second)
	defer cancel()
	
	// Get campaigns with A/B tests in progress
	rows, err := w.db.QueryContext(ctx, `
		SELECT DISTINCT campaign_id
		FROM mailing_campaign_ab_variants
		WHERE status = 'active'
		  AND is_winner = false
	`)
	if err != nil {
		log.Printf("[ABTestWorker] Error getting A/B tests: %v", err)
		return
	}
	defer rows.Close()
	
	for rows.Next() {
		var campaignID uuid.UUID
		if err := rows.Scan(&campaignID); err != nil {
			continue
		}
		
		w.analyzeABTest(ctx, campaignID)
	}
}

func (w *ABTestWorker) analyzeABTest(ctx context.Context, campaignID uuid.UUID) {
	// Get AI settings
	var autoWinner bool
	var confidenceThreshold float64
	var minSampleSize int
	var targetMetric string
	
	err := w.db.QueryRowContext(ctx, `
		SELECT enable_ab_auto_winner, ab_confidence_threshold, ab_min_sample_size, target_metric
		FROM mailing_campaign_ai_settings
		WHERE campaign_id = $1
	`, campaignID).Scan(&autoWinner, &confidenceThreshold, &minSampleSize, &targetMetric)
	
	if err == sql.ErrNoRows {
		autoWinner = true
		confidenceThreshold = 0.95
		minSampleSize = 1000
		targetMetric = "opens"
	} else if err != nil {
		return
	}
	
	if !autoWinner {
		return
	}
	
	// Get all variants for this campaign
	rows, err := w.db.QueryContext(ctx, `
		SELECT id, variant_name, is_control, sent_count, open_count, click_count, conversion_count
		FROM mailing_campaign_ab_variants
		WHERE campaign_id = $1 AND status = 'active'
		ORDER BY is_control DESC, variant_name
	`, campaignID)
	if err != nil {
		return
	}
	defer rows.Close()
	
	type variant struct {
		ID              uuid.UUID
		Name            string
		IsControl       bool
		SentCount       int
		OpenCount       int
		ClickCount      int
		ConversionCount int
	}
	
	var variants []variant
	for rows.Next() {
		var v variant
		rows.Scan(&v.ID, &v.Name, &v.IsControl, &v.SentCount, &v.OpenCount, &v.ClickCount, &v.ConversionCount)
		variants = append(variants, v)
	}
	
	if len(variants) < 2 {
		return
	}
	
	// Find control
	var control *variant
	for i := range variants {
		if variants[i].IsControl {
			control = &variants[i]
			break
		}
	}
	if control == nil {
		control = &variants[0]
	}
	
	// Check minimum sample size
	for _, v := range variants {
		if v.SentCount < minSampleSize {
			return // Not enough data yet
		}
	}
	
	// Calculate Z-scores for each variant against control
	for i := range variants {
		if variants[i].ID == control.ID {
			continue
		}
		
		var controlConversions, variantConversions int
		switch targetMetric {
		case "clicks":
			controlConversions = control.ClickCount
			variantConversions = variants[i].ClickCount
		case "conversions":
			controlConversions = control.ConversionCount
			variantConversions = variants[i].ConversionCount
		default: // opens
			controlConversions = control.OpenCount
			variantConversions = variants[i].OpenCount
		}
		
		zScore := calculateZScore(
			controlConversions, control.SentCount,
			variantConversions, variants[i].SentCount,
		)
		
		pValue := zScoreToPValue(zScore)
		confidence := 1 - pValue
		
		// Update variant with statistical data
		w.db.ExecContext(ctx, `
			UPDATE mailing_campaign_ab_variants
			SET z_score = $2, p_value = $3, confidence_level = $4, updated_at = NOW()
			WHERE id = $1
		`, variants[i].ID, zScore, pValue, confidence)
		
		// Check if we have a winner
		if confidence >= confidenceThreshold && zScore > 0 {
			// This variant wins
			w.declareWinner(ctx, campaignID, variants[i].ID, control.ID, confidence, targetMetric)
			return
		} else if confidence >= confidenceThreshold && zScore < 0 {
			// Control wins against this variant - mark variant as loser
			w.db.ExecContext(ctx, `
				UPDATE mailing_campaign_ab_variants
				SET status = 'loser', updated_at = NOW()
				WHERE id = $1
			`, variants[i].ID)
		}
	}
}

func (w *ABTestWorker) declareWinner(ctx context.Context, campaignID, winnerID, controlID uuid.UUID, confidence float64, metric string) {
	log.Printf("[ABTestWorker] Declaring winner for campaign %s: variant %s with %.2f%% confidence",
		campaignID, winnerID, confidence*100)
	
	// Mark winner
	_, err := w.db.ExecContext(ctx, `
		UPDATE mailing_campaign_ab_variants
		SET is_winner = true, status = 'winner', declared_winner_at = NOW(), updated_at = NOW()
		WHERE id = $1
	`, winnerID)
	if err != nil {
		log.Printf("[ABTestWorker] Error marking winner: %v", err)
		return
	}
	
	// Mark control/others as losers
	_, err = w.db.ExecContext(ctx, `
		UPDATE mailing_campaign_ab_variants
		SET status = 'loser', updated_at = NOW()
		WHERE campaign_id = $1 AND id != $2 AND status = 'active'
	`, campaignID, winnerID)
	if err != nil {
		log.Printf("[ABTestWorker] Error marking losers: %v", err)
	}
	
	// Get winner details for routing
	var winnerValue, winnerType string
	w.db.QueryRowContext(ctx, `
		SELECT variant_value, variant_type
		FROM mailing_campaign_ab_variants
		WHERE id = $1
	`, winnerID).Scan(&winnerValue, &winnerType)
	
	// Route remaining sends to winner
	// This updates the campaign with the winning variant's content
	switch winnerType {
	case "subject":
		w.db.ExecContext(ctx, `
			UPDATE mailing_campaigns
			SET subject = $2, updated_at = NOW()
			WHERE id = $1
		`, campaignID, winnerValue)
	case "from_name":
		w.db.ExecContext(ctx, `
			UPDATE mailing_campaigns
			SET from_name = $2, updated_at = NOW()
			WHERE id = $1
		`, campaignID, winnerValue)
	}
	
	// Log AI decision
	_, err = w.db.ExecContext(ctx, `
		INSERT INTO mailing_ai_decisions (
			campaign_id, decision_type, decision_reason, old_value, new_value,
			metrics_snapshot, ai_model, confidence, applied, applied_at
		) VALUES (
			$1, 'variant_winner', $2, $3, $4,
			'{}', 'statistical', $5, true, NOW()
		)
	`, campaignID,
		fmt.Sprintf("A/B test winner declared with %.2f%% confidence on %s metric", confidence*100, metric),
		controlID.String(),
		winnerID.String(),
		confidence,
	)
	
	if err != nil {
		log.Printf("[ABTestWorker] Error logging decision: %v", err)
	}
	
	// Create alert/notification
	w.db.ExecContext(ctx, `
		INSERT INTO mailing_campaign_alerts (
			campaign_id, alert_type, severity, title, message, auto_action_taken
		) VALUES ($1, 'variant_winner', 'info', 'A/B Test Winner Declared', $2, 'winner_selected')
	`, campaignID, fmt.Sprintf("Variant won with %.2f%% confidence. Remaining sends will use winning content.", confidence*100))
}

// calculateZScore calculates Z-score for two proportions
func calculateZScore(control, controlTotal, variant, variantTotal int) float64 {
	if controlTotal == 0 || variantTotal == 0 {
		return 0
	}
	
	p1 := float64(control) / float64(controlTotal)
	p2 := float64(variant) / float64(variantTotal)
	
	pPooled := float64(control+variant) / float64(controlTotal+variantTotal)
	
	se := math.Sqrt(pPooled * (1 - pPooled) * (1.0/float64(controlTotal) + 1.0/float64(variantTotal)))
	
	if se == 0 {
		return 0
	}
	
	return (p2 - p1) / se
}

// zScoreToPValue converts Z-score to approximate P-value
func zScoreToPValue(z float64) float64 {
	absZ := math.Abs(z)
	
	if absZ > 3.5 {
		return 0.00001
	}
	if absZ >= 2.576 {
		return 0.01
	}
	if absZ >= 1.96 {
		return 0.05
	}
	if absZ >= 1.645 {
		return 0.10
	}
	if absZ >= 1.28 {
		return 0.20
	}
	return 0.50
}

// min and max are now builtin functions in Go 1.21+
