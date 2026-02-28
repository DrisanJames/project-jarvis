package kanban

import (
	"context"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/datainjections"
	"github.com/ignite/sparkpost-monitor/internal/everflow"
	"github.com/ignite/sparkpost-monitor/internal/sparkpost"
)

// CollectorSet holds references to all collectors for AI analysis
type CollectorSet struct {
	SparkPost      *sparkpost.Collector
	Everflow       *everflow.Collector
	DataInjections *datainjections.Service
}

// AIAnalyzer analyzes ecosystem data and generates tasks
type AIAnalyzer struct {
	service    *Service
	collectors *CollectorSet
	config     Config
}

// NewAIAnalyzer creates a new AI analyzer
func NewAIAnalyzer(service *Service, collectors *CollectorSet, config Config) *AIAnalyzer {
	return &AIAnalyzer{
		service:    service,
		collectors: collectors,
		config:     config,
	}
}

// AnalyzeAndGenerateTasks performs ecosystem analysis and generates tasks
func (a *AIAnalyzer) AnalyzeAndGenerateTasks(ctx context.Context) (*AIAnalysisResult, error) {
	return a.analyzeWithInterval(ctx, a.config.AIRunInterval)
}

// analyzeWithInterval performs analysis with a specific rate limit interval
func (a *AIAnalyzer) analyzeWithInterval(ctx context.Context, interval time.Duration) (*AIAnalysisResult, error) {
	result := &AIAnalysisResult{
		NewTasks:     []Card{},
		AnalyzedAt:   time.Now(),
		NextRunAfter: time.Now().Add(a.config.AIRunInterval),
	}

	// Check if we can run
	canRun, waitTime, err := a.service.CanRunAIAnalysisWithInterval(ctx, interval)
	if err != nil {
		return nil, fmt.Errorf("checking AI run eligibility: %w", err)
	}

	if !canRun {
		result.RateLimited = true
		if waitTime > 0 {
			result.NextRunAfter = time.Now().Add(waitTime)
		}
		log.Printf("Kanban AI: Rate limited, next run after %v", result.NextRunAfter)
		return result, nil
	}

	// Get active issues for deduplication
	activeIssues, err := a.service.GetActiveIssues(ctx)
	if err != nil {
		log.Printf("Kanban AI: Error getting active issues: %v", err)
		activeIssues = make(map[string]string)
	}

	// Collect potential tasks from ecosystem health analysis
	var potentialTasks []Card

	if a.collectors != nil {
		// 1. Deliverability Infrastructure Health (IPs, ISPs, Domains)
		deliverabilityTasks := a.analyzeDeliverabilityInfrastructure(ctx, activeIssues)
		potentialTasks = append(potentialTasks, deliverabilityTasks...)

		// 2. Aggregate Ecosystem Health (bounce trends, engagement trends)
		ecosystemTasks := a.analyzeAggregateEcosystemHealth(ctx, activeIssues)
		potentialTasks = append(potentialTasks, ecosystemTasks...)

		// 3. Data Pipeline Infrastructure
		pipelineTasks := a.analyzeDataPipeline(ctx, activeIssues)
		potentialTasks = append(potentialTasks, pipelineTasks...)

		// 4. Revenue Infrastructure (strategic, not tactical)
		revenueTasks := a.analyzeRevenueInfrastructure(ctx, activeIssues)
		potentialTasks = append(potentialTasks, revenueTasks...)
	}

	// Filter duplicates and prioritize
	result.NewTasks = a.filterAndPrioritize(potentialTasks, activeIssues)
	result.SkippedCount = len(potentialTasks) - len(result.NewTasks)

	// Add tasks to board
	for _, task := range result.NewTasks {
		if err := a.service.AddAIGeneratedCard(ctx, task); err != nil {
			log.Printf("Kanban AI: Error adding task '%s': %v", task.Title, err)
		} else {
			log.Printf("Kanban AI: Created task '%s' (priority: %s)", task.Title, task.Priority)
		}
	}

	// Update last run time
	a.service.UpdateAIRunTime(ctx)

	log.Printf("Kanban AI: Analysis complete - %d new tasks, %d skipped", len(result.NewTasks), result.SkippedCount)
	return result, nil
}

// analyzeDeliverabilityInfrastructure checks IP, ISP, and domain health
func (a *AIAnalyzer) analyzeDeliverabilityInfrastructure(ctx context.Context, activeIssues map[string]string) []Card {
	var tasks []Card

	if a.collectors.SparkPost == nil {
		return tasks
	}

	// Check IP Health
	ipMetrics := a.collectors.SparkPost.GetLatestIPMetrics()
	for _, ip := range ipMetrics {
		// Flag IPs with critical or warning status
		if ip.Status == "critical" || ip.Status == "warning" {
			fingerprint := GenerateFingerprint(SourceDeliverability, "ip_health", ip.IP)
			if _, exists := activeIssues[fingerprint]; exists {
				continue
			}

			priority := PriorityHigh
			if ip.Status == "critical" {
				priority = PriorityCritical
			}

			// Build detailed bounce breakdown
			bounceBreakdown := ""
			if ip.Metrics.Sent > 0 {
				bounceBreakdown = fmt.Sprintf("Hard: %d (%.2f%%), Soft: %d (%.2f%%), Blocks: %d (%.2f%%)",
					ip.Metrics.HardBounced, ip.Metrics.HardBounceRate*100,
					ip.Metrics.SoftBounced, ip.Metrics.SoftBounceRate*100,
					ip.Metrics.BlockBounced, ip.Metrics.BlockRate*100)
			}

			// Build description with location context
			description := fmt.Sprintf("Sending IP %s has %s status.\n\n**Issue:** %s\n**Location:** IP %s (Pool: %s, ESP: SparkPost)\n**Volume:** %d sent, %d bounced (%.2f%%)\n**Breakdown:** %s\n\nReview IP reputation, check blocklist status, and consider warmup adjustments or throttling.",
				ip.IP, ip.Status, ip.StatusReason,
				ip.IP, ip.Pool, ip.Metrics.Sent, ip.Metrics.Bounced, ip.Metrics.BounceRate*100,
				bounceBreakdown)

			tasks = append(tasks, Card{
				Title:       fmt.Sprintf("[IP %s] %s: %s", ip.IP, ip.Status, ip.StatusReason),
				Description: description,
				Priority:    priority,
				IssueFingerprint: fingerprint,
				Labels:      []string{"deliverability", "ip-reputation", "infrastructure", ip.Pool},
				AIContext: &AIContext{
					Source:     SourceDeliverability,
					Reasoning:  fmt.Sprintf("IP %s showing degraded health which impacts inbox placement across all sends.", ip.IP),
					EntityType: "sending_ip",
					EntityID:   ip.IP,
					Severity:   priority,
					DataPoints: map[string]string{
						"ip":               ip.IP,
						"pool":             ip.Pool,
						"status":           ip.Status,
						"status_reason":    ip.StatusReason,
						"delivery_rate":    fmt.Sprintf("%.2f%%", ip.Metrics.DeliveryRate*100),
						"bounce_rate":      fmt.Sprintf("%.2f%%", ip.Metrics.BounceRate*100),
						"hard_bounce_rate": fmt.Sprintf("%.2f%%", ip.Metrics.HardBounceRate*100),
						"soft_bounce_rate": fmt.Sprintf("%.2f%%", ip.Metrics.SoftBounceRate*100),
						"block_rate":       fmt.Sprintf("%.2f%%", ip.Metrics.BlockRate*100),
						"sent":             fmt.Sprintf("%d", ip.Metrics.Sent),
						"bounced":          fmt.Sprintf("%d", ip.Metrics.Bounced),
					},
					GeneratedAt: time.Now(),
				},
			})
		}

		// Check for elevated block rates on specific IP
		if ip.Metrics.Sent > 1000 && ip.Metrics.BlockRate > 0.02 { // >2% block rate
			fingerprint := GenerateFingerprint(SourceDeliverability, "ip_blocks", ip.IP)
			if _, exists := activeIssues[fingerprint]; exists {
				continue
			}

			// Determine priority based on block rate
			blockPriority := PriorityHigh
			if ip.Metrics.BlockRate > 0.05 { // >5% is critical
				blockPriority = PriorityCritical
			}

			description := fmt.Sprintf("IP %s has elevated block rate.\n\n**Location:** IP %s (Pool: %s, ESP: SparkPost)\n**Block Rate:** %.2f%% (%d blocks out of %d sent)\n\nCheck blocklist status (Spamhaus, Barracuda, etc.) and review ISP-specific sending patterns.",
				ip.IP, ip.IP, ip.Pool, ip.Metrics.BlockRate*100, ip.Metrics.BlockBounced, ip.Metrics.Sent)

			tasks = append(tasks, Card{
				Title:       fmt.Sprintf("[IP %s] Elevated Blocks: %.1f%%", ip.IP, ip.Metrics.BlockRate*100),
				Description: description,
				Priority:    blockPriority,
				IssueFingerprint: fingerprint,
				Labels:      []string{"deliverability", "blocks", "ip-reputation", ip.Pool},
				AIContext: &AIContext{
					Source:     SourceDeliverability,
					Reasoning:  "Elevated block rate indicates reputation issues that affect all mail sent from this IP.",
					EntityType: "sending_ip",
					EntityID:   ip.IP,
					Severity:   blockPriority,
					DataPoints: map[string]string{
						"ip":         ip.IP,
						"pool":       ip.Pool,
						"block_rate": fmt.Sprintf("%.2f%%", ip.Metrics.BlockRate*100),
						"blocks":     fmt.Sprintf("%d", ip.Metrics.BlockBounced),
						"sent":       fmt.Sprintf("%d", ip.Metrics.Sent),
					},
					GeneratedAt: time.Now(),
				},
			})
		}
	}

	// Check ISP-specific Health (Yahoo, Gmail, Microsoft deferrals/blocks)
	ispMetrics := a.collectors.SparkPost.GetLatestISPMetrics()
	for _, isp := range ispMetrics {
		// Flag ISPs with critical or warning status
		if isp.Status == "critical" || isp.Status == "warning" {
			fingerprint := GenerateFingerprint(SourceDeliverability, "isp_health", isp.Provider)
			if _, exists := activeIssues[fingerprint]; exists {
				continue
			}

			priority := PriorityHigh
			if isp.Status == "critical" {
				priority = PriorityCritical
			}

			// Build detailed bounce breakdown
			bounceBreakdown := ""
			if isp.Metrics.Sent > 0 {
				bounceBreakdown = fmt.Sprintf("Hard bounces: %d (%.2f%%), Soft bounces: %d (%.2f%%), Blocks: %d (%.2f%%)",
					isp.Metrics.HardBounced, isp.Metrics.HardBounceRate*100,
					isp.Metrics.SoftBounced, isp.Metrics.SoftBounceRate*100,
					isp.Metrics.BlockBounced, isp.Metrics.BlockRate*100)
			}

			// Build description with ISP-specific context
			description := fmt.Sprintf("%s showing %s status. %s.\n\n**Bounce Location:** %s mailbox provider (ESP: SparkPost)\n**Volume:** %d sent, %d bounced\n**Breakdown:** %s\n\nReview sending practices and list hygiene specifically for %s subscribers.",
				isp.Provider, isp.Status, isp.StatusReason,
				isp.Provider, isp.Metrics.Sent, isp.Metrics.Bounced,
				bounceBreakdown, isp.Provider)

			tasks = append(tasks, Card{
				Title:       fmt.Sprintf("[%s] %s: %s", isp.Provider, isp.Status, isp.StatusReason),
				Description: description,
				Priority:    priority,
				IssueFingerprint: fingerprint,
				Labels:      []string{"deliverability", "isp", isp.Provider, "bounce-investigation"},
				AIContext: &AIContext{
					Source:     SourceDeliverability,
					Reasoning:  fmt.Sprintf("%s is a major mailbox provider. Degraded deliverability here impacts a significant portion of the audience.", isp.Provider),
					EntityType: "isp",
					EntityID:   isp.Provider,
					Severity:   priority,
					DataPoints: map[string]string{
						"isp":              isp.Provider,
						"status":           isp.Status,
						"status_reason":    isp.StatusReason,
						"delivery_rate":    fmt.Sprintf("%.2f%%", isp.Metrics.DeliveryRate*100),
						"bounce_rate":      fmt.Sprintf("%.2f%%", isp.Metrics.BounceRate*100),
						"hard_bounce_rate": fmt.Sprintf("%.2f%%", isp.Metrics.HardBounceRate*100),
						"soft_bounce_rate": fmt.Sprintf("%.2f%%", isp.Metrics.SoftBounceRate*100),
						"block_rate":       fmt.Sprintf("%.2f%%", isp.Metrics.BlockRate*100),
						"sent":             fmt.Sprintf("%d", isp.Metrics.Sent),
						"bounced":          fmt.Sprintf("%d", isp.Metrics.Bounced),
					},
					GeneratedAt: time.Now(),
				},
			})
		}

		// Check for elevated deferrals (delays) at specific ISP
		if isp.Metrics.Sent > 5000 && isp.Metrics.Delayed > 0 {
			deferralRate := float64(isp.Metrics.Delayed) / float64(isp.Metrics.Sent)
			if deferralRate > 0.05 { // >5% deferral rate
				fingerprint := GenerateFingerprint(SourceDeliverability, "isp_deferrals", isp.Provider)
				if _, exists := activeIssues[fingerprint]; exists {
					continue
				}

				tasks = append(tasks, Card{
					Title:       fmt.Sprintf("%s Deferrals Elevated (%.1f%%)", isp.Provider, deferralRate*100),
					Description: fmt.Sprintf("%s is deferring %.1f%% of mail. This indicates throttling or reputation concerns. Consider reducing volume or improving engagement.", isp.Provider, deferralRate*100),
					Priority:    PriorityHigh,
					IssueFingerprint: fingerprint,
					Labels:      []string{"deliverability", "deferrals", "throttling", isp.Provider},
					AIContext: &AIContext{
						Source:     SourceDeliverability,
						Reasoning:  fmt.Sprintf("%s deferrals suggest the ISP is rate-limiting or has reputation concerns with our sending.", isp.Provider),
						EntityType: "isp",
						EntityID:   isp.Provider,
						Severity:   PriorityHigh,
						DataPoints: map[string]string{
							"deferral_rate": fmt.Sprintf("%.2f%%", deferralRate*100),
							"deferred":      fmt.Sprintf("%d", isp.Metrics.Delayed),
							"sent":          fmt.Sprintf("%d", isp.Metrics.Sent),
						},
						GeneratedAt: time.Now(),
					},
				})
			}
		}
	}

	// Check Domain Health
	domainMetrics := a.collectors.SparkPost.GetLatestDomainMetrics()
	for _, domain := range domainMetrics {
		if domain.Status == "critical" || domain.Status == "warning" {
			fingerprint := GenerateFingerprint(SourceDeliverability, "domain_health", domain.Domain)
			if _, exists := activeIssues[fingerprint]; exists {
				continue
			}

			priority := PriorityHigh
			if domain.Status == "critical" {
				priority = PriorityCritical
			}

			// Build detailed bounce breakdown
			bounceBreakdown := ""
			if domain.Metrics.Sent > 0 {
				bounceBreakdown = fmt.Sprintf("Hard: %d (%.2f%%), Soft: %d (%.2f%%), Blocks: %d (%.2f%%)",
					domain.Metrics.HardBounced, domain.Metrics.HardBounceRate*100,
					domain.Metrics.SoftBounced, domain.Metrics.SoftBounceRate*100,
					domain.Metrics.BlockBounced, domain.Metrics.BlockRate*100)
			}

			description := fmt.Sprintf("Sending domain %s has %s status.\n\n**Issue:** %s\n**Location:** Domain %s (ESP: SparkPost)\n**Volume:** %d sent, %d bounced (%.2f%%)\n**Breakdown:** %s\n\nReview DMARC/SPF/DKIM configuration and domain reputation.",
				domain.Domain, domain.Status, domain.StatusReason,
				domain.Domain, domain.Metrics.Sent, domain.Metrics.Bounced, domain.Metrics.BounceRate*100,
				bounceBreakdown)

			tasks = append(tasks, Card{
				Title:       fmt.Sprintf("[Domain %s] %s: %s", domain.Domain, domain.Status, domain.StatusReason),
				Description: description,
				Priority:    priority,
				IssueFingerprint: fingerprint,
				Labels:      []string{"deliverability", "domain", "authentication"},
				AIContext: &AIContext{
					Source:     SourceDeliverability,
					Reasoning:  "Domain health issues can affect authentication and reputation across all sends from this domain.",
					EntityType: "domain",
					EntityID:   domain.Domain,
					Severity:   priority,
					DataPoints: map[string]string{
						"domain":           domain.Domain,
						"status":           domain.Status,
						"status_reason":    domain.StatusReason,
						"delivery_rate":    fmt.Sprintf("%.2f%%", domain.Metrics.DeliveryRate*100),
						"bounce_rate":      fmt.Sprintf("%.2f%%", domain.Metrics.BounceRate*100),
						"hard_bounce_rate": fmt.Sprintf("%.2f%%", domain.Metrics.HardBounceRate*100),
						"soft_bounce_rate": fmt.Sprintf("%.2f%%", domain.Metrics.SoftBounceRate*100),
						"block_rate":       fmt.Sprintf("%.2f%%", domain.Metrics.BlockRate*100),
						"sent":             fmt.Sprintf("%d", domain.Metrics.Sent),
						"bounced":          fmt.Sprintf("%d", domain.Metrics.Bounced),
					},
					GeneratedAt: time.Now(),
				},
			})
		}
	}

	return tasks
}

// analyzeAggregateEcosystemHealth creates systemic review tasks based on aggregate patterns
func (a *AIAnalyzer) analyzeAggregateEcosystemHealth(ctx context.Context, activeIssues map[string]string) []Card {
	var tasks []Card

	if a.collectors.SparkPost == nil {
		return tasks
	}

	summary := a.collectors.SparkPost.GetLatestSummary()
	if summary == nil {
		return tasks
	}

	// Check overall bounce rate trend (systemic hygiene issue)
	if summary.BounceRate > 0.05 { // >5% overall bounce rate
		fingerprint := GenerateFingerprint(SourceDeliverability, "hygiene", "aggregate_bounces")
		if _, exists := activeIssues[fingerprint]; !exists {
			priority := PriorityHigh
			if summary.BounceRate > 0.08 {
				priority = PriorityCritical
			}

			// Build ISP-specific breakdown to show where bounces are occurring
			ispBreakdown := a.buildISPBounceBreakdown()
			
			description := fmt.Sprintf("Overall bounce rate exceeds healthy threshold.\n\n**Aggregate Stats:** %.2f%% bounce rate (%d bounced out of %d sent)\n\n**Bounce Location by ISP:**\n%s\nReview list acquisition sources, implement stricter validation, and audit suppression lists. Focus on the ISPs with highest bounce rates first.",
				summary.BounceRate*100, summary.TotalBounced, summary.TotalDelivered+summary.TotalBounced, ispBreakdown)

			tasks = append(tasks, Card{
				Title:       fmt.Sprintf("List Hygiene Review: %.1f%% Aggregate Bounce Rate", summary.BounceRate*100),
				Description: description,
				Priority:    priority,
				IssueFingerprint: fingerprint,
				Labels:      []string{"hygiene", "bounces", "list-quality"},
				AIContext: &AIContext{
					Source:     SourceDeliverability,
					Reasoning:  "Elevated aggregate bounce rate indicates systemic list quality issues that affect sender reputation.",
					EntityType: "ecosystem",
					EntityID:   "aggregate_bounce_rate",
					Severity:   priority,
					DataPoints: map[string]string{
						"bounce_rate":     fmt.Sprintf("%.2f%%", summary.BounceRate*100),
						"total_bounced":   fmt.Sprintf("%d", summary.TotalBounced),
						"total_sent":      fmt.Sprintf("%d", summary.TotalDelivered+summary.TotalBounced),
					},
					GeneratedAt: time.Now(),
				},
			})
		}
	}

	// Check complaint rate (systemic engagement issue)
	if summary.ComplaintRate > 0.001 { // >0.1% complaint rate
		fingerprint := GenerateFingerprint(SourceDeliverability, "complaints", "aggregate")
		if _, exists := activeIssues[fingerprint]; !exists {
			priority := PriorityHigh
			if summary.ComplaintRate > 0.002 {
				priority = PriorityCritical
			}

			tasks = append(tasks, Card{
				Title:       fmt.Sprintf("Complaint Rate Elevated: %.3f%%", summary.ComplaintRate*100),
				Description: "Spam complaint rate exceeds threshold. Review email frequency, content relevance, and unsubscribe visibility. High complaints damage sender reputation.",
				Priority:    priority,
				IssueFingerprint: fingerprint,
				Labels:      []string{"complaints", "reputation", "engagement"},
				AIContext: &AIContext{
					Source:     SourceDeliverability,
					Reasoning:  "Complaint rates above 0.1% indicate recipients marking mail as spam, which damages reputation with ISPs.",
					EntityType: "ecosystem",
					EntityID:   "complaint_rate",
					Severity:   priority,
					DataPoints: map[string]string{
						"complaint_rate": fmt.Sprintf("%.3f%%", summary.ComplaintRate*100),
						"total_complaints": fmt.Sprintf("%d", summary.TotalComplaints),
					},
					GeneratedAt: time.Now(),
				},
			})
		}
	}

	// Check engagement trends (declining open rates = deliverability risk)
	if summary.OpenRateChange < -10 { // Open rate dropped >10%
		fingerprint := GenerateFingerprint(SourceDeliverability, "engagement", "declining_opens")
		if _, exists := activeIssues[fingerprint]; !exists {
			tasks = append(tasks, Card{
				Title:       fmt.Sprintf("Engagement Declining: Open Rate Down %.1f%%", -summary.OpenRateChange),
				Description: "Open rates are trending down significantly. This may indicate deliverability issues (inbox placement) or content relevance problems. Audit sending patterns and content strategy.",
				Priority:    PriorityNormal,
				IssueFingerprint: fingerprint,
				Labels:      []string{"engagement", "opens", "content"},
				AIContext: &AIContext{
					Source:     SourceDeliverability,
					Reasoning:  "Declining engagement signals either deliverability problems (mail going to spam) or content that doesn't resonate.",
					EntityType: "ecosystem",
					EntityID:   "engagement_trend",
					Severity:   PriorityNormal,
					DataPoints: map[string]string{
						"open_rate":        fmt.Sprintf("%.2f%%", summary.OpenRate*100),
						"open_rate_change": fmt.Sprintf("%.1f%%", summary.OpenRateChange),
					},
					GeneratedAt: time.Now(),
				},
			})
		}
	}

	return tasks
}

// analyzeDataPipeline checks data infrastructure health
func (a *AIAnalyzer) analyzeDataPipeline(ctx context.Context, activeIssues map[string]string) []Card {
	var tasks []Card

	if a.collectors.DataInjections == nil {
		return tasks
	}

	dashboard := a.collectors.DataInjections.GetDashboard()
	if dashboard == nil {
		return tasks
	}

	// CRITICAL: Check System Health - Processor not running
	if dashboard.Ingestion != nil && dashboard.Ingestion.SystemHealth != nil {
		sysHealth := dashboard.Ingestion.SystemHealth
		if !sysHealth.ProcessorRunning {
			fingerprint := GenerateFingerprint(SourceDataPipeline, "processor", "not_running")
			if _, exists := activeIssues[fingerprint]; !exists {
				tasks = append(tasks, Card{
					Title:       fmt.Sprintf("CRITICAL: Data Processor Down (%.1fh)", sysHealth.HoursSinceHydration),
					Description: fmt.Sprintf("No data has been hydrated in %.1f hours. The processor should hydrate at least one data set every hour. Investigate immediately.", sysHealth.HoursSinceHydration),
					Priority:    PriorityCritical,
					IssueFingerprint: fingerprint,
					Labels:      []string{"data-pipeline", "processor", "critical", "infrastructure"},
					AIContext: &AIContext{
						Source:     SourceDataPipeline,
						Reasoning:  "Processor down means no new data is being processed. This is a system-level failure affecting all data feeds.",
						EntityType: "system",
						EntityID:   "processor",
						Severity:   PriorityCritical,
						DataPoints: map[string]string{
							"hours_since_hydration": fmt.Sprintf("%.1f", sysHealth.HoursSinceHydration),
							"last_hydration":        sysHealth.LastHydrationTime.Format(time.RFC3339),
						},
						GeneratedAt: time.Now(),
					},
				})
			}
		}
	}

	// Partner-level alerts (less critical than system health)
	if dashboard.Ingestion != nil && len(dashboard.Ingestion.PartnerAlerts) > 0 {
		// Only create task if multiple partners have significant gaps (>48h)
		criticalPartners := 0
		for _, alert := range dashboard.Ingestion.PartnerAlerts {
			if alert.GapHours > 48 {
				criticalPartners++
			}
		}
		
		if criticalPartners >= 2 {
			fingerprint := GenerateFingerprint(SourceDataPipeline, "partners", "multiple_gaps")
			if _, exists := activeIssues[fingerprint]; !exists {
				tasks = append(tasks, Card{
					Title:       fmt.Sprintf("Multiple Data Partners Offline (%d)", criticalPartners),
					Description: fmt.Sprintf("%d data partners have not sent data in over 48 hours. Review partner integrations and contact vendors.", criticalPartners),
					Priority:    PriorityHigh,
					IssueFingerprint: fingerprint,
					Labels:      []string{"data-pipeline", "partners", "vendor"},
					AIContext: &AIContext{
						Source:     SourceDataPipeline,
						Reasoning:  "Multiple partner feeds offline affects data freshness and list quality.",
						EntityType: "partners",
						EntityID:   "multiple",
						Severity:   PriorityHigh,
						DataPoints: map[string]string{
							"partners_offline": fmt.Sprintf("%d", criticalPartners),
							"total_alerts":     fmt.Sprintf("%d", len(dashboard.Ingestion.PartnerAlerts)),
						},
						GeneratedAt: time.Now(),
					},
				})
			}
		}
	}

	// Check Snowflake validation health
	if dashboard.Validation != nil && dashboard.Validation.TodayRecords == 0 && dashboard.Validation.TotalRecords > 0 {
		fingerprint := GenerateFingerprint(SourceDataPipeline, "validation", "pipeline_stalled")
		if _, exists := activeIssues[fingerprint]; !exists {
			tasks = append(tasks, Card{
				Title:       "Email Validation Pipeline Stalled",
				Description: "No email validations processed today. Check Snowflake connection and validation job status.",
				Priority:    PriorityHigh,
				IssueFingerprint: fingerprint,
				Labels:      []string{"data-pipeline", "validation", "infrastructure"},
				AIContext: &AIContext{
					Source:     SourceDataPipeline,
					Reasoning:  "Validation pipeline is critical for list hygiene - stalled processing means bad addresses slip through.",
					EntityType: "pipeline",
					EntityID:   "validation",
					Severity:   PriorityHigh,
					DataPoints: map[string]string{
						"total_records": fmt.Sprintf("%d", dashboard.Validation.TotalRecords),
						"today_records": "0",
					},
					GeneratedAt: time.Now(),
				},
			})
		}
	}

	// Check Ongage import health
	if dashboard.Import != nil && dashboard.Import.InProgress > 3 {
		fingerprint := GenerateFingerprint(SourceDataPipeline, "import", "backlog")
		if _, exists := activeIssues[fingerprint]; !exists {
			tasks = append(tasks, Card{
				Title:       fmt.Sprintf("Import Queue Backlog: %d jobs stuck", dashboard.Import.InProgress),
				Description: "Multiple Ongage imports stuck in progress. Check API status and processing capacity.",
				Priority:    PriorityHigh,
				IssueFingerprint: fingerprint,
				Labels:      []string{"data-pipeline", "ongage", "imports"},
				AIContext: &AIContext{
					Source:     SourceDataPipeline,
					Reasoning:  "Import backlogs delay list updates and can cause sending to outdated lists.",
					EntityType: "pipeline",
					EntityID:   "imports",
					Severity:   PriorityHigh,
					DataPoints: map[string]string{
						"in_progress": fmt.Sprintf("%d", dashboard.Import.InProgress),
					},
					GeneratedAt: time.Now(),
				},
			})
		}
	}

	return tasks
}

// buildISPBounceBreakdown builds a string showing bounce breakdown by ISP
func (a *AIAnalyzer) buildISPBounceBreakdown() string {
	if a.collectors.SparkPost == nil {
		return "ISP data not available"
	}

	ispMetrics := a.collectors.SparkPost.GetLatestISPMetrics()
	if len(ispMetrics) == 0 {
		return "No ISP data available"
	}

	// Sort ISPs by bounce rate (highest first)
	sort.Slice(ispMetrics, func(i, j int) bool {
		return ispMetrics[i].Metrics.BounceRate > ispMetrics[j].Metrics.BounceRate
	})

	var breakdown string
	for i, isp := range ispMetrics {
		if i >= 5 { // Show top 5 ISPs
			break
		}
		if isp.Metrics.Sent == 0 {
			continue
		}
		breakdown += fmt.Sprintf("- **%s**: %.2f%% bounce rate (%d bounced / %d sent)\n",
			isp.Provider, isp.Metrics.BounceRate*100, isp.Metrics.Bounced, isp.Metrics.Sent)
	}

	if breakdown == "" {
		return "No significant ISP bounce data"
	}
	return breakdown
}

// analyzeRevenueInfrastructure checks strategic revenue health (not tactical)
func (a *AIAnalyzer) analyzeRevenueInfrastructure(ctx context.Context, activeIssues map[string]string) []Card {
	var tasks []Card

	if a.collectors.Everflow == nil {
		return tasks
	}

	// Check for significant revenue trends (strategic, not per-property)
	daily := a.collectors.Everflow.GetDailyPerformance()
	if len(daily) >= 7 {
		// Calculate week-over-week trend
		thisWeek := 0.0
		lastWeek := 0.0
		for i := len(daily) - 1; i >= len(daily)-7 && i >= 0; i-- {
			thisWeek += daily[i].Revenue
		}
		for i := len(daily) - 8; i >= len(daily)-14 && i >= 0; i-- {
			lastWeek += daily[i].Revenue
		}

		if lastWeek > 0 {
			weekChange := (thisWeek - lastWeek) / lastWeek * 100
			if weekChange < -20 { // >20% WoW decline
				fingerprint := GenerateFingerprint(SourceRevenue, "trend", "weekly_decline")
				if _, exists := activeIssues[fingerprint]; !exists {
					priority := PriorityHigh
					if weekChange < -35 {
						priority = PriorityCritical
					}

					tasks = append(tasks, Card{
						Title:       fmt.Sprintf("Revenue Trend Down %.0f%% WoW", -weekChange),
						Description: fmt.Sprintf("Weekly revenue ($%.0f) is down %.0f%% from last week ($%.0f). Investigate traffic quality, offer mix, and deliverability impact.", thisWeek, -weekChange, lastWeek),
						Priority:    priority,
						IssueFingerprint: fingerprint,
						Labels:      []string{"revenue", "trend", "strategic"},
						AIContext: &AIContext{
							Source:     SourceRevenue,
							Reasoning:  "Significant revenue decline warrants investigation of root causes: traffic quality, offer performance, or deliverability issues affecting reach.",
							EntityType: "revenue",
							EntityID:   "weekly_trend",
							Severity:   priority,
							DataPoints: map[string]string{
								"this_week":     fmt.Sprintf("$%.2f", thisWeek),
								"last_week":     fmt.Sprintf("$%.2f", lastWeek),
								"change_percent": fmt.Sprintf("%.1f%%", weekChange),
							},
							GeneratedAt: time.Now(),
						},
					})
				}
			}
		}
	}

	return tasks
}

// filterAndPrioritize filters duplicates and limits tasks
func (a *AIAnalyzer) filterAndPrioritize(tasks []Card, activeIssues map[string]string) []Card {
	// Remove duplicates
	seen := make(map[string]bool)
	var unique []Card
	for _, task := range tasks {
		if task.IssueFingerprint != "" {
			if seen[task.IssueFingerprint] {
				continue
			}
			if _, exists := activeIssues[task.IssueFingerprint]; exists {
				continue
			}
			seen[task.IssueFingerprint] = true
		}
		unique = append(unique, task)
	}

	// Sort by priority (critical first, then high, then normal)
	sort.Slice(unique, func(i, j int) bool {
		priorityOrder := map[string]int{
			PriorityCritical: 0,
			PriorityHigh:     1,
			PriorityNormal:   2,
		}
		return priorityOrder[unique[i].Priority] < priorityOrder[unique[j].Priority]
	})

	// Limit to max new tasks per run
	if len(unique) > a.config.MaxNewTasksPerRun {
		unique = unique[:a.config.MaxNewTasksPerRun]
	}

	return unique
}

// ManualTrigger allows manual triggering of AI analysis (bypasses time check)
func (a *AIAnalyzer) ManualTrigger(ctx context.Context) (*AIAnalysisResult, error) {
	// Use interval of 0 to bypass rate limiting
	return a.analyzeWithInterval(ctx, 0)
}
