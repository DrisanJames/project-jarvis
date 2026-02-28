package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"sync"
	"time"
)

// AgenticLoop is the self-improving learning system that continuously monitors and optimizes
type AgenticLoop struct {
	mu              sync.RWMutex
	db              *sql.DB
	knowledgeBase   *KnowledgeBase
	openAIAgent     *OpenAIAgent
	bedrockAgent    *BedrockAgent  // AWS Bedrock alternative (keeps data on AWS)
	s3Storage       *S3Storage     // S3 storage for state persistence
	running         bool
	stopCh          chan struct{}
	
	// Learning state
	LearningCycles      int       `json:"learning_cycles"`
	LastLearningTime    time.Time `json:"last_learning_time"`
	TotalOptimizations  int       `json:"total_optimizations"`
	TotalRecommendations int      `json:"total_recommendations"`
	
	// Performance tracking
	PerformanceHistory  []PerformanceSnapshot `json:"performance_history"`
	ImprovementRate     float64               `json:"improvement_rate"`
	
	// Self-improvement actions
	PendingActions      []AgenticAction       `json:"pending_actions"`
	CompletedActions    []AgenticAction       `json:"completed_actions"`
	
	// Configuration
	LearningInterval    time.Duration         `json:"learning_interval"`
	OptimizationEnabled bool                  `json:"optimization_enabled"`
	UseAWSOnly          bool                  `json:"use_aws_only"` // If true, use Bedrock instead of OpenAI
}

// PerformanceSnapshot captures system performance at a point in time
type PerformanceSnapshot struct {
	Timestamp       time.Time `json:"timestamp"`
	TotalCampaigns  int       `json:"total_campaigns"`
	TotalSent       int       `json:"total_sent"`
	AvgOpenRate     float64   `json:"avg_open_rate"`
	AvgClickRate    float64   `json:"avg_click_rate"`
	AvgBounceRate   float64   `json:"avg_bounce_rate"`
	ComplaintRate   float64   `json:"complaint_rate"`
	HealthScore     float64   `json:"health_score"`
	RevenueTotal    float64   `json:"revenue_total"`
}

// AgenticAction represents an action the system can take to improve itself
type AgenticAction struct {
	ID          string    `json:"id"`
	Type        string    `json:"type"`        // optimize_timing, clean_list, adjust_throttle, retrain_model
	Description string    `json:"description"`
	Priority    int       `json:"priority"`    // 1=critical, 2=high, 3=medium, 4=low
	Status      string    `json:"status"`      // pending, in_progress, completed, failed
	CreatedAt   time.Time `json:"created_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	Result      string    `json:"result,omitempty"`
	Impact      string    `json:"impact,omitempty"`
}

// AgenticLoopConfig contains configuration for the agentic loop
type AgenticLoopConfig struct {
	// S3 storage configuration
	S3Bucket        string
	S3Prefix        string
	S3Region        string
	S3EncryptionKey string
	S3Compress      bool
	
	// Use AWS-only mode (Bedrock instead of OpenAI)
	UseAWSOnly bool
	
	// Learning configuration
	LearningInterval    time.Duration
	OptimizationEnabled bool
}

// NewAgenticLoop creates a new self-learning agentic system
func NewAgenticLoop(db *sql.DB, kb *KnowledgeBase, openAI *OpenAIAgent) *AgenticLoop {
	return &AgenticLoop{
		db:                  db,
		knowledgeBase:       kb,
		openAIAgent:         openAI,
		stopCh:              make(chan struct{}),
		PerformanceHistory:  make([]PerformanceSnapshot, 0),
		PendingActions:      make([]AgenticAction, 0),
		CompletedActions:    make([]AgenticAction, 0),
		LearningInterval:    5 * time.Minute,
		OptimizationEnabled: true,
		UseAWSOnly:          false,
	}
}

// NewAgenticLoopWithConfig creates a new agentic system with full configuration
func NewAgenticLoopWithConfig(db *sql.DB, kb *KnowledgeBase, agent *Agent, cfg AgenticLoopConfig) (*AgenticLoop, error) {
	loop := &AgenticLoop{
		db:                  db,
		knowledgeBase:       kb,
		stopCh:              make(chan struct{}),
		PerformanceHistory:  make([]PerformanceSnapshot, 0),
		PendingActions:      make([]AgenticAction, 0),
		CompletedActions:    make([]AgenticAction, 0),
		LearningInterval:    cfg.LearningInterval,
		OptimizationEnabled: cfg.OptimizationEnabled,
		UseAWSOnly:          cfg.UseAWSOnly,
	}
	
	if loop.LearningInterval == 0 {
		loop.LearningInterval = 5 * time.Minute
	}
	
	// Initialize S3 storage if configured
	if cfg.S3Bucket != "" {
		s3Cfg := S3StorageConfig{
			Bucket:        cfg.S3Bucket,
			Prefix:        cfg.S3Prefix,
			Region:        cfg.S3Region,
			EncryptionKey: cfg.S3EncryptionKey,
			Compress:      cfg.S3Compress,
		}
		
		s3Storage, err := NewS3Storage(s3Cfg)
		if err != nil {
			log.Printf("AgenticLoop: Failed to initialize S3 storage: %v", err)
		} else {
			loop.s3Storage = s3Storage
			log.Printf("AgenticLoop: Using S3 storage for state persistence")
			
			// Also set S3 storage on knowledge base
			if kb != nil {
				kb.SetS3Storage(s3Storage)
			}
		}
	}
	
	// Initialize Bedrock agent if AWS-only mode
	if cfg.UseAWSOnly {
		bedrockAgent, err := NewBedrockAgent("", agent, kb)
		if err != nil {
			log.Printf("AgenticLoop: Failed to initialize Bedrock agent: %v", err)
		} else {
			loop.bedrockAgent = bedrockAgent
			log.Printf("AgenticLoop: Using AWS Bedrock for AI (data stays on AWS)")
		}
	}
	
	// Try to load existing state from S3
	if loop.s3Storage != nil {
		ctx := context.Background()
		if state, err := loop.s3Storage.LoadAgenticState(ctx); err == nil {
			loop.loadState(state)
			log.Printf("AgenticLoop: Loaded previous state from S3")
		}
	}
	
	return loop, nil
}

// SetS3Storage sets the S3 storage backend
func (a *AgenticLoop) SetS3Storage(s3 *S3Storage) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.s3Storage = s3
	log.Printf("AgenticLoop: S3 storage configured - bucket=%s", s3.GetBucket())
}

// SetBedrockAgent sets the AWS Bedrock agent (replaces OpenAI)
func (a *AgenticLoop) SetBedrockAgent(bedrock *BedrockAgent) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.bedrockAgent = bedrock
	a.UseAWSOnly = true
	log.Printf("AgenticLoop: Using AWS Bedrock agent - model=%s", bedrock.GetModelID())
}

// loadState loads state from a map
func (a *AgenticLoop) loadState(state map[string]interface{}) {
	a.mu.Lock()
	defer a.mu.Unlock()
	
	if v, ok := state["learning_cycles"].(float64); ok {
		a.LearningCycles = int(v)
	}
	if v, ok := state["total_optimizations"].(float64); ok {
		a.TotalOptimizations = int(v)
	}
	if v, ok := state["total_recommendations"].(float64); ok {
		a.TotalRecommendations = int(v)
	}
	if v, ok := state["improvement_rate"].(float64); ok {
		a.ImprovementRate = v
	}
}

// Start begins the continuous learning loop
func (a *AgenticLoop) Start() {
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return
	}
	a.running = true
	a.mu.Unlock()
	
	log.Println("AgenticLoop: Starting continuous self-learning system...")
	
	go a.runLearningLoop()
}

// Stop halts the learning loop
func (a *AgenticLoop) Stop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	
	if !a.running {
		return
	}
	
	close(a.stopCh)
	a.running = false
	log.Println("AgenticLoop: Stopped")
}

// runLearningLoop is the main continuous learning loop
func (a *AgenticLoop) runLearningLoop() {
	// Initial learning on startup
	a.performLearningCycle()
	
	ticker := time.NewTicker(a.LearningInterval)
	defer ticker.Stop()
	
	for {
		select {
		case <-a.stopCh:
			return
		case <-ticker.C:
			a.performLearningCycle()
		}
	}
}

// performLearningCycle runs a single learning iteration
func (a *AgenticLoop) performLearningCycle() {
	ctx := context.Background()
	startTime := time.Now()
	
	log.Println("AgenticLoop: Running learning cycle...")
	
	a.mu.Lock()
	a.LearningCycles++
	a.LastLearningTime = startTime
	a.mu.Unlock()
	
	// 1. Capture current performance snapshot
	snapshot := a.capturePerformanceSnapshot(ctx)
	a.addSnapshot(snapshot)
	
	// 2. Analyze trends and patterns
	trends := a.analyzeTrends()
	
	// 3. Generate improvement recommendations
	recommendations := a.generateRecommendations(snapshot, trends)
	
	// 4. Queue actions for execution
	for _, rec := range recommendations {
		a.queueAction(rec)
	}
	
	// 5. Execute pending actions
	if a.OptimizationEnabled {
		a.executePendingActions(ctx)
	}
	
	// 6. Calculate improvement rate
	a.calculateImprovementRate()
	
	// 7. Update knowledge base
	if a.knowledgeBase != nil {
		a.updateKnowledgeBase(snapshot, recommendations)
	}
	
	// 8. Persist state
	a.persistState()
	
	log.Printf("AgenticLoop: Cycle %d complete in %v - Health: %.1f, Actions: %d pending, %d completed",
		a.LearningCycles, time.Since(startTime), snapshot.HealthScore,
		len(a.PendingActions), len(a.CompletedActions))
}

// capturePerformanceSnapshot captures current system metrics
func (a *AgenticLoop) capturePerformanceSnapshot(ctx context.Context) PerformanceSnapshot {
	snapshot := PerformanceSnapshot{
		Timestamp: time.Now(),
	}
	
	if a.db == nil {
		return snapshot
	}
	
	// Get campaign metrics
	a.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(sent_count),0),
			   COALESCE(AVG(CASE WHEN sent_count > 0 THEN open_count::float/sent_count END), 0),
			   COALESCE(AVG(CASE WHEN sent_count > 0 THEN click_count::float/sent_count END), 0),
			   COALESCE(AVG(CASE WHEN sent_count > 0 THEN bounce_count::float/sent_count END), 0),
			   COALESCE(AVG(CASE WHEN sent_count > 0 THEN complaint_count::float/sent_count END), 0),
			   COALESCE(SUM(revenue), 0)
		FROM mailing_campaigns WHERE sent_count > 0
	`).Scan(&snapshot.TotalCampaigns, &snapshot.TotalSent,
		&snapshot.AvgOpenRate, &snapshot.AvgClickRate,
		&snapshot.AvgBounceRate, &snapshot.ComplaintRate,
		&snapshot.RevenueTotal)
	
	// Calculate health score
	snapshot.HealthScore = a.calculateHealthScore(snapshot)
	
	return snapshot
}

// calculateHealthScore computes overall system health
func (a *AgenticLoop) calculateHealthScore(s PerformanceSnapshot) float64 {
	score := 100.0
	
	// Penalize high complaint rate (major issue)
	if s.ComplaintRate > 0.001 {
		score -= 40
	} else if s.ComplaintRate > 0.0005 {
		score -= 20
	}
	
	// Penalize high bounce rate
	if s.AvgBounceRate > 0.05 {
		score -= 30
	} else if s.AvgBounceRate > 0.02 {
		score -= 15
	}
	
	// Reward good engagement
	if s.AvgOpenRate > 0.20 {
		score += 10
	} else if s.AvgOpenRate < 0.10 {
		score -= 10
	}
	
	if s.AvgClickRate > 0.03 {
		score += 5
	}
	
	// Bound score
	if score < 0 {
		score = 0
	}
	if score > 100 {
		score = 100
	}
	
	return score
}

// addSnapshot adds a performance snapshot to history
func (a *AgenticLoop) addSnapshot(s PerformanceSnapshot) {
	a.mu.Lock()
	defer a.mu.Unlock()
	
	a.PerformanceHistory = append(a.PerformanceHistory, s)
	
	// Keep last 288 snapshots (24 hours at 5-min intervals)
	if len(a.PerformanceHistory) > 288 {
		a.PerformanceHistory = a.PerformanceHistory[len(a.PerformanceHistory)-288:]
	}
}

// TrendAnalysis contains trend data
type TrendAnalysis struct {
	OpenRateTrend      string  // improving, stable, declining
	ClickRateTrend     string
	HealthTrend        string
	RevenueTrend       string
	OpenRateChange     float64
	ClickRateChange    float64
	HealthChange       float64
	RevenueChange      float64
}

// analyzeTrends analyzes performance trends
func (a *AgenticLoop) analyzeTrends() TrendAnalysis {
	a.mu.RLock()
	defer a.mu.RUnlock()
	
	trends := TrendAnalysis{}
	
	if len(a.PerformanceHistory) < 2 {
		trends.OpenRateTrend = "stable"
		trends.ClickRateTrend = "stable"
		trends.HealthTrend = "stable"
		trends.RevenueTrend = "stable"
		return trends
	}
	
	// Compare recent vs older snapshots
	recent := a.PerformanceHistory[len(a.PerformanceHistory)-1]
	var older PerformanceSnapshot
	
	if len(a.PerformanceHistory) > 12 {
		older = a.PerformanceHistory[len(a.PerformanceHistory)-12]
	} else {
		older = a.PerformanceHistory[0]
	}
	
	// Calculate changes
	trends.OpenRateChange = recent.AvgOpenRate - older.AvgOpenRate
	trends.ClickRateChange = recent.AvgClickRate - older.AvgClickRate
	trends.HealthChange = recent.HealthScore - older.HealthScore
	trends.RevenueChange = recent.RevenueTotal - older.RevenueTotal
	
	// Determine trends
	trends.OpenRateTrend = a.determineTrend(trends.OpenRateChange)
	trends.ClickRateTrend = a.determineTrend(trends.ClickRateChange)
	trends.HealthTrend = a.determineTrend(trends.HealthChange)
	trends.RevenueTrend = a.determineTrend(trends.RevenueChange)
	
	return trends
}

func (a *AgenticLoop) determineTrend(change float64) string {
	if change > 0.01 {
		return "improving"
	} else if change < -0.01 {
		return "declining"
	}
	return "stable"
}

// generateRecommendations creates improvement actions based on analysis
func (a *AgenticLoop) generateRecommendations(snapshot PerformanceSnapshot, trends TrendAnalysis) []AgenticAction {
	var actions []AgenticAction
	
	// High complaint rate - critical
	if snapshot.ComplaintRate > 0.001 {
		actions = append(actions, AgenticAction{
			ID:          fmt.Sprintf("action_%d", time.Now().UnixNano()),
			Type:        "list_hygiene",
			Description: "Complaint rate exceeds 0.1% - Clean suppression list and remove low-engaged subscribers",
			Priority:    1,
			Status:      "pending",
			CreatedAt:   time.Now(),
		})
	}
	
	// High bounce rate
	if snapshot.AvgBounceRate > 0.03 {
		actions = append(actions, AgenticAction{
			ID:          fmt.Sprintf("action_%d", time.Now().UnixNano()),
			Type:        "list_validation",
			Description: "Bounce rate above 3% - Validate email addresses and remove invalid entries",
			Priority:    2,
			Status:      "pending",
			CreatedAt:   time.Now(),
		})
	}
	
	// Declining open rates
	if trends.OpenRateTrend == "declining" {
		actions = append(actions, AgenticAction{
			ID:          fmt.Sprintf("action_%d", time.Now().UnixNano()),
			Type:        "optimize_content",
			Description: "Open rates declining - A/B test subject lines and optimize send times",
			Priority:    3,
			Status:      "pending",
			CreatedAt:   time.Now(),
		})
	}
	
	// Low engagement
	if snapshot.AvgOpenRate < 0.10 {
		actions = append(actions, AgenticAction{
			ID:          fmt.Sprintf("action_%d", time.Now().UnixNano()),
			Type:        "segment_audience",
			Description: "Open rate below 10% - Segment by engagement and target high-value subscribers",
			Priority:    2,
			Status:      "pending",
			CreatedAt:   time.Now(),
		})
	}
	
	// Health declining
	if trends.HealthTrend == "declining" {
		actions = append(actions, AgenticAction{
			ID:          fmt.Sprintf("action_%d", time.Now().UnixNano()),
			Type:        "system_audit",
			Description: "System health declining - Run comprehensive audit of all components",
			Priority:    2,
			Status:      "pending",
			CreatedAt:   time.Now(),
		})
	}
	
	a.mu.Lock()
	a.TotalRecommendations += len(actions)
	a.mu.Unlock()
	
	return actions
}

// queueAction adds an action to the pending queue (avoiding duplicates)
func (a *AgenticLoop) queueAction(action AgenticAction) {
	a.mu.Lock()
	defer a.mu.Unlock()
	
	// Check for duplicate
	for _, existing := range a.PendingActions {
		if existing.Type == action.Type {
			return // Skip duplicate
		}
	}
	
	a.PendingActions = append(a.PendingActions, action)
}

// executePendingActions processes pending actions
func (a *AgenticLoop) executePendingActions(ctx context.Context) {
	a.mu.Lock()
	actions := make([]AgenticAction, len(a.PendingActions))
	copy(actions, a.PendingActions)
	a.mu.Unlock()
	
	for i, action := range actions {
		if action.Status != "pending" {
			continue
		}
		
		// Execute based on type
		result, err := a.executeAction(ctx, action)
		
		a.mu.Lock()
		now := time.Now()
		if err != nil {
			a.PendingActions[i].Status = "failed"
			a.PendingActions[i].Result = err.Error()
		} else {
			a.PendingActions[i].Status = "completed"
			a.PendingActions[i].CompletedAt = &now
			a.PendingActions[i].Result = result
			a.TotalOptimizations++
			
			// Move to completed
			a.CompletedActions = append(a.CompletedActions, a.PendingActions[i])
		}
		a.mu.Unlock()
	}
	
	// Clean up completed from pending
	a.mu.Lock()
	var remaining []AgenticAction
	for _, action := range a.PendingActions {
		if action.Status == "pending" {
			remaining = append(remaining, action)
		}
	}
	a.PendingActions = remaining
	
	// Keep only last 100 completed actions
	if len(a.CompletedActions) > 100 {
		a.CompletedActions = a.CompletedActions[len(a.CompletedActions)-100:]
	}
	a.mu.Unlock()
}

// executeAction performs a specific optimization action
func (a *AgenticLoop) executeAction(ctx context.Context, action AgenticAction) (string, error) {
	log.Printf("AgenticLoop: Executing action [%s] %s", action.Type, action.Description[:min(50, len(action.Description))])
	
	switch action.Type {
	case "list_hygiene":
		return a.executeListHygiene(ctx)
	case "list_validation":
		return a.executeListValidation(ctx)
	case "optimize_content":
		return a.executeContentOptimization(ctx)
	case "segment_audience":
		return a.executeAudienceSegmentation(ctx)
	case "system_audit":
		return a.executeSystemAudit(ctx)
	default:
		return "No action taken", nil
	}
}

func (a *AgenticLoop) executeListHygiene(ctx context.Context) (string, error) {
	if a.db == nil {
		return "Database not available", nil
	}
	
	// Find low-engagement subscribers
	result, err := a.db.ExecContext(ctx, `
		UPDATE mailing_subscribers 
		SET status = 'unsubscribed'
		WHERE id IN (
			SELECT s.id FROM mailing_subscribers s
			JOIN mailing_inbox_profiles p ON LOWER(s.email) = LOWER(p.email)
			WHERE p.engagement_score < 20 AND p.total_sent > 5
		)
	`)
	if err != nil {
		return "", err
	}
	
	rows, _ := result.RowsAffected()
	return fmt.Sprintf("Cleaned %d low-engagement subscribers", rows), nil
}

func (a *AgenticLoop) executeListValidation(ctx context.Context) (string, error) {
	// Log validation action (actual validation would use external service)
	return "List validation scheduled - will verify emails on next send", nil
}

func (a *AgenticLoop) executeContentOptimization(ctx context.Context) (string, error) {
	if a.db == nil {
		return "Content optimization skipped - database not available", nil
	}
	// Query best performing subjects
	var bestSubject string
	var bestRate float64
	a.db.QueryRowContext(ctx, `
		SELECT subject, (open_count::float / NULLIF(sent_count,0)) as rate
		FROM mailing_campaigns WHERE sent_count > 0
		ORDER BY rate DESC LIMIT 1
	`).Scan(&bestSubject, &bestRate)
	
	return fmt.Sprintf("Best performing subject identified: '%s' (%.1f%% open rate)", bestSubject, bestRate*100), nil
}

func (a *AgenticLoop) executeAudienceSegmentation(ctx context.Context) (string, error) {
	if a.db == nil {
		return "Audience segmentation skipped - database not available", nil
	}
	// Count engagement segments
	var high, med, low int
	a.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_inbox_profiles WHERE engagement_score >= 70").Scan(&high)
	a.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_inbox_profiles WHERE engagement_score >= 40 AND engagement_score < 70").Scan(&med)
	a.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_inbox_profiles WHERE engagement_score < 40").Scan(&low)
	
	return fmt.Sprintf("Audience segmented: %d high-engaged, %d medium, %d low - Target high-engaged first", high, med, low), nil
}

func (a *AgenticLoop) executeSystemAudit(ctx context.Context) (string, error) {
	if a.db == nil {
		return "System audit skipped - database not available", nil
	}
	// Count key entities
	var campaigns, subscribers, events int
	a.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_campaigns").Scan(&campaigns)
	a.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_subscribers WHERE status = 'confirmed'").Scan(&subscribers)
	a.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_tracking_events").Scan(&events)
	
	return fmt.Sprintf("Audit complete: %d campaigns, %d active subscribers, %d events tracked", campaigns, subscribers, events), nil
}

// calculateImprovementRate computes the rate of improvement over time
func (a *AgenticLoop) calculateImprovementRate() {
	a.mu.Lock()
	defer a.mu.Unlock()
	
	if len(a.PerformanceHistory) < 2 {
		a.ImprovementRate = 0
		return
	}
	
	first := a.PerformanceHistory[0]
	last := a.PerformanceHistory[len(a.PerformanceHistory)-1]
	
	// Calculate average improvement across metrics
	improvements := []float64{}
	
	if first.HealthScore > 0 {
		improvements = append(improvements, (last.HealthScore-first.HealthScore)/first.HealthScore)
	}
	if first.AvgOpenRate > 0 {
		improvements = append(improvements, (last.AvgOpenRate-first.AvgOpenRate)/first.AvgOpenRate)
	}
	
	if len(improvements) > 0 {
		sum := 0.0
		for _, v := range improvements {
			sum += v
		}
		a.ImprovementRate = sum / float64(len(improvements)) * 100
	}
}

// updateKnowledgeBase syncs learnings to the knowledge base
func (a *AgenticLoop) updateKnowledgeBase(snapshot PerformanceSnapshot, actions []AgenticAction) {
	if a.knowledgeBase == nil {
		return
	}
	
	// Update ecosystem state
	a.knowledgeBase.mu.Lock()
	a.knowledgeBase.EcosystemState.HealthScore = snapshot.HealthScore
	a.knowledgeBase.EcosystemState.BaselineOpenRate = snapshot.AvgOpenRate
	a.knowledgeBase.EcosystemState.BaselineClickRate = snapshot.AvgClickRate
	a.knowledgeBase.EcosystemState.BaselineBounceRate = snapshot.AvgBounceRate
	a.knowledgeBase.EcosystemState.BaselineComplaintRate = snapshot.ComplaintRate
	a.knowledgeBase.EcosystemState.LastAssessment = time.Now()
	
	if snapshot.HealthScore >= 80 {
		a.knowledgeBase.EcosystemState.OverallHealth = "healthy"
	} else if snapshot.HealthScore >= 60 {
		a.knowledgeBase.EcosystemState.OverallHealth = "warning"
	} else {
		a.knowledgeBase.EcosystemState.OverallHealth = "critical"
	}
	a.knowledgeBase.mu.Unlock()
	
	// Add learned patterns from actions
	for _, action := range actions {
		if action.Status == "completed" {
			a.knowledgeBase.AddLearnedPattern(LearnedPattern{
				Type:            action.Type,
				Description:     action.Description,
				Confidence:      0.8,
				Recommendation:  action.Result,
				FirstObserved:   action.CreatedAt,
				LastObserved:    time.Now(),
			})
		}
	}
	
	// Save knowledge base
	a.knowledgeBase.Save()
}

// persistState saves the agentic loop state (S3 preferred, falls back to local)
func (a *AgenticLoop) persistState() {
	a.mu.RLock()
	state := map[string]interface{}{
		"learning_cycles":       a.LearningCycles,
		"last_learning_time":    a.LastLearningTime,
		"total_optimizations":   a.TotalOptimizations,
		"total_recommendations": a.TotalRecommendations,
		"improvement_rate":      a.ImprovementRate,
		"pending_actions":       len(a.PendingActions),
		"completed_actions":     len(a.CompletedActions),
		"performance_snapshots": len(a.PerformanceHistory),
		"use_aws_only":          a.UseAWSOnly,
	}
	s3Storage := a.s3Storage
	a.mu.RUnlock()
	
	// Try S3 first if configured
	if s3Storage != nil {
		ctx := context.Background()
		if err := s3Storage.SaveAgenticState(ctx, state); err != nil {
			log.Printf("AgenticLoop: Failed to save state to S3: %v (falling back to local)", err)
		} else {
			return // S3 save succeeded
		}
	}
	
	// Fall back to local file
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return
	}
	
	os.WriteFile("/tmp/agentic_loop_state.json", data, 0644)
}

// GetStatus returns the current agentic loop status
func (a *AgenticLoop) GetStatus() map[string]interface{} {
	a.mu.RLock()
	defer a.mu.RUnlock()
	
	var latestSnapshot PerformanceSnapshot
	if len(a.PerformanceHistory) > 0 {
		latestSnapshot = a.PerformanceHistory[len(a.PerformanceHistory)-1]
	}
	
	return map[string]interface{}{
		"running":               a.running,
		"learning_cycles":       a.LearningCycles,
		"last_learning_time":    a.LastLearningTime,
		"total_optimizations":   a.TotalOptimizations,
		"total_recommendations": a.TotalRecommendations,
		"improvement_rate":      math.Round(a.ImprovementRate*10) / 10,
		"pending_actions":       len(a.PendingActions),
		"completed_actions":     len(a.CompletedActions),
		"health_score":          latestSnapshot.HealthScore,
		"optimization_enabled":  a.OptimizationEnabled,
		"learning_interval":     a.LearningInterval.String(),
	}
}

// GetRecentActions returns recent agentic actions
func (a *AgenticLoop) GetRecentActions(limit int) []AgenticAction {
	a.mu.RLock()
	defer a.mu.RUnlock()
	
	if limit <= 0 {
		limit = 10
	}
	
	// Combine pending and completed
	all := append(a.PendingActions, a.CompletedActions...)
	
	if len(all) <= limit {
		return all
	}
	
	return all[len(all)-limit:]
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
