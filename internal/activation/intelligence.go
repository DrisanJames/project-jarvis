package activation

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// DataActivationIntelligence is the main engine that analyzes ecosystem
// sending data and generates ISP-specific activation recommendations.
// Built by Jarvis (Opus 4.6).
type DataActivationIntelligence struct {
	ispThresholds map[ISP]ISPThresholds
}

// NewDataActivationIntelligence creates a new instance with ISP-specific thresholds
func NewDataActivationIntelligence() *DataActivationIntelligence {
	return &DataActivationIntelligence{
		ispThresholds: map[ISP]ISPThresholds{
			ISPGmail: {
				MaxBounceRate:     2.0,
				MaxComplaintRate:  0.08,
				MinEngagementRate: 15.0,
				MaxSpamTrapRate:   0.01,
				MaxUnsubRate:      0.5,
			},
			ISPYahoo: {
				MaxBounceRate:     3.0,
				MaxComplaintRate:  0.05,
				MinEngagementRate: 10.0,
				MaxSpamTrapRate:   0.02,
				MaxUnsubRate:      0.8,
			},
			ISPOutlook: {
				MaxBounceRate:     2.5,
				MaxComplaintRate:  0.1,
				MinEngagementRate: 12.0,
				MaxSpamTrapRate:   0.01,
				MaxUnsubRate:      0.6,
			},
			ISPAOL: {
				MaxBounceRate:     3.0,
				MaxComplaintRate:  0.05,
				MinEngagementRate: 8.0,
				MaxSpamTrapRate:   0.02,
				MaxUnsubRate:      1.0,
			},
			ISPApple: {
				MaxBounceRate:     2.0,
				MaxComplaintRate:  0.08,
				MinEngagementRate: 10.0,
				MaxSpamTrapRate:   0.01,
				MaxUnsubRate:      0.5,
			},
			ISPOther: {
				MaxBounceRate:     3.0,
				MaxComplaintRate:  0.1,
				MinEngagementRate: 8.0,
				MaxSpamTrapRate:   0.02,
				MaxUnsubRate:      1.0,
			},
		},
	}
}

// AnalyzeAndRecommend performs full analysis: health scoring + recommendation generation
func (dai *DataActivationIntelligence) AnalyzeAndRecommend(ispData map[ISP]ISPSendingData) *ActivationSnapshot {
	snapshot := &ActivationSnapshot{
		Timestamp:     time.Now(),
		HealthScores:  make(map[ISP]DataHealthScore),
		ISPStrategies: make(map[ISP]ISPActivationStrategy),
	}

	var totalVolume int64
	var weightedScoreSum float64
	var totalWeight float64

	// Step 1: Compute health scores per ISP
	for isp, data := range ispData {
		score := dai.computeISPHealthScore(isp, data)
		snapshot.HealthScores[isp] = score
		totalVolume += data.TotalSent

		weight := float64(data.TotalSent)
		weightedScoreSum += score.OverallScore * weight
		totalWeight += weight
	}

	snapshot.TotalSendingVolume = totalVolume
	if totalWeight > 0 {
		snapshot.OverallHealth = math.Round(weightedScoreSum/totalWeight*10) / 10
	}
	snapshot.OverallRisk = computeRiskLevel(snapshot.OverallHealth)

	// Step 2: Generate ISP-specific strategies
	for isp, score := range snapshot.HealthScores {
		strategy := dai.generateISPStrategy(isp, score)
		snapshot.ISPStrategies[isp] = strategy
	}

	// Step 3: Generate activation recommendations
	snapshot.Recommendations = dai.generateActivationRecommendations(snapshot.HealthScores, snapshot.ISPStrategies)

	// Step 4: Generate summary
	snapshot.Summary = dai.generateSummary(snapshot)

	return snapshot
}

// computeISPHealthScore calculates health metrics for a single ISP
func (dai *DataActivationIntelligence) computeISPHealthScore(isp ISP, data ISPSendingData) DataHealthScore {
	score := DataHealthScore{
		ISP:            isp,
		ISPDisplayName: ispDisplayName(isp),
		TotalVolume:    data.TotalSent,
		Diagnostics:    []string{},
	}

	if data.TotalSent == 0 {
		score.OverallScore = 0
		score.RiskLevel = RiskCritical
		score.Diagnostics = append(score.Diagnostics,
			fmt.Sprintf("No sending volume detected for %s — this ISP segment needs activation from scratch", ispDisplayName(isp)))
		return score
	}

	totalSentF := float64(data.TotalSent)
	deliveredF := float64(data.Delivered)

	score.BounceRate = safePercent(float64(data.Bounced), totalSentF)
	score.HardBounceRate = safePercent(float64(data.HardBounced), totalSentF)
	score.ComplaintRate = safePercent(float64(data.Complaints), totalSentF)
	score.UnsubscribeRate = safePercent(float64(data.Unsubscribes), totalSentF)
	score.SpamTrapRate = safePercent(float64(data.SpamTraps), totalSentF)
	score.DeliveryRate = safePercent(deliveredF, totalSentF)

	if deliveredF > 0 {
		score.EngagementRate = safePercent(float64(data.UniqueOpens), deliveredF)
		if data.UniqueOpens > 0 {
			score.ClickToOpenRate = safePercent(float64(data.UniqueClicks), float64(data.UniqueOpens))
		}
	}

	thresholds := dai.getThresholds(isp)

	// Weighted scoring: Bounce 30pts, Complaint 35pts, Engagement 35pts
	score.BounceScoreImpact = computeBounceImpact(score.BounceRate, score.HardBounceRate, thresholds.MaxBounceRate)
	score.ComplaintScoreImpact = computeComplaintImpact(score.ComplaintRate, score.SpamTrapRate, thresholds.MaxComplaintRate, thresholds.MaxSpamTrapRate)
	score.EngagementScoreImpact = computeEngagementImpact(score.EngagementRate, score.ClickToOpenRate, thresholds.MinEngagementRate)

	score.OverallScore = math.Min(100, math.Max(0,
		score.BounceScoreImpact+score.ComplaintScoreImpact+score.EngagementScoreImpact))

	score.RiskLevel = computeRiskLevel(score.OverallScore)
	score.Diagnostics = dai.generateDiagnostics(isp, score, thresholds)

	return score
}

// generateActivationRecommendations creates diverse, ISP-specific recommendations
func (dai *DataActivationIntelligence) generateActivationRecommendations(
	healthScores map[ISP]DataHealthScore,
	strategies map[ISP]ISPActivationStrategy,
) []ActivationRecommendation {
	var recs []ActivationRecommendation
	recID := 0

	for _, isp := range AllISPs() {
		score, hasScore := healthScores[isp]
		if !hasScore {
			continue
		}

		strategy, hasStrategy := strategies[isp]
		if !hasStrategy {
			continue
		}

		recID++
		rec := ActivationRecommendation{
			ID:             fmt.Sprintf("act-%d", recID),
			ISP:            isp,
			ISPDisplayName: ispDisplayName(isp),
			StrategyType:   strategy.StrategyType,
			HealthScore:    score,
			Strategy:       strategy,
		}

		// Determine priority and build recommendation text
		switch score.RiskLevel {
		case RiskCritical:
			rec.Priority = PriorityCritical
			rec.Title = fmt.Sprintf("CRITICAL: %s Activation Required", ispDisplayName(isp))
			rec.Description = fmt.Sprintf(
				"%s health score is %.0f/100 (Critical Risk). "+
					"Immediate action needed to establish or repair sending reputation. "+
					"%s", ispDisplayName(isp), score.OverallScore, strategy.Description)
			rec.ImpactEstimate = "High — without intervention, deliverability will continue to decline"
			rec.TimelineEstimate = fmt.Sprintf("%d days to stabilize", strategy.EstimatedDays)
		case RiskHigh:
			rec.Priority = PriorityHigh
			rec.Title = fmt.Sprintf("HIGH: %s Reputation Repair Needed", ispDisplayName(isp))
			rec.Description = fmt.Sprintf(
				"%s health score is %.0f/100 (High Risk). "+
					"Key metrics are outside acceptable thresholds. "+
					"%s", ispDisplayName(isp), score.OverallScore, strategy.Description)
			rec.ImpactEstimate = "Significant — current metrics risk throttling or blocking"
			rec.TimelineEstimate = fmt.Sprintf("%d days to reach healthy thresholds", strategy.EstimatedDays)
		case RiskMedium:
			rec.Priority = PriorityMedium
			rec.Title = fmt.Sprintf("MODERATE: %s Optimisation Opportunity", ispDisplayName(isp))
			rec.Description = fmt.Sprintf(
				"%s health score is %.0f/100 (Medium Risk). "+
					"Performance is functional but below optimal. "+
					"%s", ispDisplayName(isp), score.OverallScore, strategy.Description)
			rec.ImpactEstimate = "Moderate — optimization will improve inbox placement and ROI"
			rec.TimelineEstimate = fmt.Sprintf("%d days to optimize", strategy.EstimatedDays)
		default:
			rec.Priority = PriorityLow
			rec.Title = fmt.Sprintf("MAINTAIN: %s Performance is Healthy", ispDisplayName(isp))
			rec.Description = fmt.Sprintf(
				"%s health score is %.0f/100 (Healthy). "+
					"Continue current practices. "+
					"%s", ispDisplayName(isp), score.OverallScore, strategy.Description)
			rec.ImpactEstimate = "Low — maintain current healthy metrics"
			rec.TimelineEstimate = "Ongoing maintenance"
		}

		// Generate a campaign suggestion for this ISP
		rec.CampaignSuggestion = dai.generateCampaignSuggestion(isp, score, strategy)

		recs = append(recs, rec)
	}

	// Sort by priority (critical first) then by health score ascending (worst first)
	priorityOrder := map[Priority]int{
		PriorityCritical: 0, PriorityHigh: 1, PriorityMedium: 2, PriorityLow: 3,
	}
	sort.Slice(recs, func(i, j int) bool {
		pi, pj := priorityOrder[recs[i].Priority], priorityOrder[recs[j].Priority]
		if pi != pj {
			return pi < pj
		}
		return recs[i].HealthScore.OverallScore < recs[j].HealthScore.OverallScore
	})

	return recs
}

// generateISPStrategy creates a detailed activation strategy for a specific ISP
func (dai *DataActivationIntelligence) generateISPStrategy(isp ISP, score DataHealthScore) ISPActivationStrategy {
	strategy := ISPActivationStrategy{
		ISP:            isp,
		ISPDisplayName: ispDisplayName(isp),
	}

	// Determine strategy type based on health score
	switch {
	case score.TotalVolume == 0:
		strategy.StrategyType = StrategyWarmup
		strategy.StrategyName = fmt.Sprintf("%s Domain Warmup", ispDisplayName(isp))
		strategy.EstimatedDays = 30
		strategy.DifficultyLevel = "moderate"
	case score.ComplaintRate > dai.getThresholds(isp).MaxComplaintRate*2:
		strategy.StrategyType = StrategyComplaintControl
		strategy.StrategyName = fmt.Sprintf("%s Complaint Rate Emergency", ispDisplayName(isp))
		strategy.EstimatedDays = 14
		strategy.DifficultyLevel = "hard"
	case score.BounceRate > dai.getThresholds(isp).MaxBounceRate*2:
		strategy.StrategyType = StrategyListHygiene
		strategy.StrategyName = fmt.Sprintf("%s List Hygiene Campaign", ispDisplayName(isp))
		strategy.EstimatedDays = 7
		strategy.DifficultyLevel = "moderate"
	case score.EngagementRate < dai.getThresholds(isp).MinEngagementRate*0.5:
		strategy.StrategyType = StrategyEngagementRamp
		strategy.StrategyName = fmt.Sprintf("%s Engagement Recovery", ispDisplayName(isp))
		strategy.EstimatedDays = 21
		strategy.DifficultyLevel = "moderate"
	case score.OverallScore < 45:
		strategy.StrategyType = StrategyRepRepair
		strategy.StrategyName = fmt.Sprintf("%s Reputation Repair", ispDisplayName(isp))
		strategy.EstimatedDays = 28
		strategy.DifficultyLevel = "hard"
	case score.OverallScore < 65:
		strategy.StrategyType = StrategyEngagementRamp
		strategy.StrategyName = fmt.Sprintf("%s Engagement Optimisation", ispDisplayName(isp))
		strategy.EstimatedDays = 14
		strategy.DifficultyLevel = "easy"
	default:
		strategy.StrategyType = StrategyVolumeScaling
		strategy.StrategyName = fmt.Sprintf("%s Volume Scaling", ispDisplayName(isp))
		strategy.EstimatedDays = 14
		strategy.DifficultyLevel = "easy"
	}

	// ISP-specific strategy content
	strategy.Description = getISPStrategyDescription(isp, strategy.StrategyType, score)
	strategy.KeyActions = getISPKeyActions(isp, strategy.StrategyType, score)
	strategy.WarmupSchedule = generateWarmupSchedule(isp, strategy.StrategyType, score)
	strategy.Risks = getISPRisks(isp, strategy.StrategyType)
	strategy.SuccessMetrics = getISPSuccessMetrics(isp)

	return strategy
}

// generateCampaignSuggestion provides a ready-to-configure campaign for this ISP
func (dai *DataActivationIntelligence) generateCampaignSuggestion(isp ISP, score DataHealthScore, strategy ISPActivationStrategy) CampaignSuggestion {
	suggestion := CampaignSuggestion{}

	switch isp {
	case ISPGmail:
		suggestion.CampaignName = "Gmail Activation — Engagement First"
		suggestion.TargetSegment = "Gmail 7-Day Openers"
		suggestion.SegmentCriteria = "email LIKE '%@gmail.com' AND last_open_date >= NOW() - INTERVAL 7 DAY"
		suggestion.SubjectLines = []string{
			"Exclusive for you — don't miss this",
			"Quick update: something special inside",
			"{FirstName}, we picked this just for you",
		}
		suggestion.SendSchedule = "10:00 AM - 12:00 PM local time, Mon-Thu"
		suggestion.ESPRecommended = "SparkPost (highest Gmail deliverability)"
		suggestion.Volume = "Start at 5,000/day, increase 20% daily if bounce < 2%"
		suggestion.Notes = "Gmail rewards engagement above all. Start with most engaged segment, then expand. " +
			"Monitor Promotions tab placement — aim for Primary inbox."

	case ISPYahoo:
		suggestion.CampaignName = "Yahoo Activation — Complaint Control"
		suggestion.TargetSegment = "Yahoo 14-Day Clickers"
		suggestion.SegmentCriteria = "email LIKE '%@yahoo.com' AND last_click_date >= NOW() - INTERVAL 14 DAY"
		suggestion.SubjectLines = []string{
			"Your weekly picks are ready",
			"Don't miss out — limited time",
			"We think you'll love this, {FirstName}",
		}
		suggestion.SendSchedule = "9:00 AM - 11:00 AM local time, Tue-Fri"
		suggestion.ESPRecommended = "Mailgun (strong Yahoo feedback loop integration)"
		suggestion.Volume = "Start at 3,000/day, increase 15% daily if complaints < 0.05%"
		suggestion.Notes = "Yahoo is complaint-rate sensitive. Always include visible unsubscribe. " +
			"Register for Yahoo CFL and process removals in real-time. Avoid link shorteners."

	case ISPOutlook:
		suggestion.CampaignName = "Outlook Activation — Authentication & Trust"
		suggestion.TargetSegment = "Outlook 30-Day Engaged"
		suggestion.SegmentCriteria = "(email LIKE '%@outlook.com' OR email LIKE '%@hotmail.com') AND (last_open_date >= NOW() - INTERVAL 30 DAY OR last_click_date >= NOW() - INTERVAL 30 DAY)"
		suggestion.SubjectLines = []string{
			"{FirstName}, your personalized update",
			"Important: your account has something new",
			"Selected for you — see what's inside",
		}
		suggestion.SendSchedule = "8:00 AM - 10:00 AM local time, Mon-Wed"
		suggestion.ESPRecommended = "SES (consistent Outlook/Microsoft deliverability)"
		suggestion.Volume = "Start at 2,000/day, increase 25% every 3 days with clean metrics"
		suggestion.Notes = "Ensure DMARC alignment (p=reject). Apply to SNDS and JMRP. " +
			"Personalise subject lines — SmartScreen rewards 1-to-1 patterns. Keep HTML minimal."

	case ISPAOL:
		suggestion.CampaignName = "AOL Activation — Accessibility & Simplicity"
		suggestion.TargetSegment = "AOL 14-Day Active"
		suggestion.SegmentCriteria = "email LIKE '%@aol.com' AND last_open_date >= NOW() - INTERVAL 14 DAY"
		suggestion.SubjectLines = []string{
			"Simple savings just for you",
			"Easy to read, easy to save",
			"Your update is inside — take a look",
		}
		suggestion.SendSchedule = "6:00 PM - 9:00 PM EST (evening engagement window)"
		suggestion.ESPRecommended = "Mailgun (shared Yahoo infrastructure handling)"
		suggestion.Volume = "Start at 1,000/day, increase 10% daily"
		suggestion.Notes = "AOL audience skews older — use 16px+ fonts, high-contrast buttons, " +
			"single-column layouts. Keep images under 100KB. Re-confirm inactive addresses every 90 days."

	case ISPApple:
		suggestion.CampaignName = "Apple Mail Activation — Click-Based Strategy"
		suggestion.TargetSegment = "Apple Mail 14-Day Clickers"
		suggestion.SegmentCriteria = "(email LIKE '%@icloud.com' OR email LIKE '%@me.com') AND last_click_date >= NOW() - INTERVAL 14 DAY"
		suggestion.SubjectLines = []string{
			"Tap to discover what's new",
			"Your curated picks are ready",
			"Something worth clicking — see inside",
		}
		suggestion.SendSchedule = "12:00 PM - 2:00 PM local time (lunch browsing window)"
		suggestion.ESPRecommended = "SparkPost (strong Apple authentication support)"
		suggestion.Volume = "Start at 2,000/day, increase 20% daily"
		suggestion.Notes = "Apple MPP inflates opens — do NOT use open rate as success metric. " +
			"Measure clicks, conversions, and replies. Ensure SPF/DKIM/DMARC fully aligned. " +
			"Test dark mode rendering."

	default:
		suggestion.CampaignName = "General ISP Activation"
		suggestion.TargetSegment = "30-Day Engaged Subscribers"
		suggestion.SegmentCriteria = "last_open_date >= NOW() - INTERVAL 30 DAY"
		suggestion.SubjectLines = []string{
			"Something new for you today",
			"Don't miss this — just for you",
		}
		suggestion.SendSchedule = "10:00 AM - 12:00 PM local time"
		suggestion.ESPRecommended = "Any configured ESP"
		suggestion.Volume = "Start at 2,000/day, standard warmup"
		suggestion.Notes = "Follow standard deliverability best practices."
	}

	return suggestion
}

// generateSummary creates a human-readable summary of the activation intelligence
func (dai *DataActivationIntelligence) generateSummary(snapshot *ActivationSnapshot) string {
	criticalCount := 0
	highCount := 0
	healthyCount := 0

	for _, score := range snapshot.HealthScores {
		switch score.RiskLevel {
		case RiskCritical:
			criticalCount++
		case RiskHigh:
			highCount++
		case RiskHealthy, RiskLow:
			healthyCount++
		}
	}

	if criticalCount > 0 {
		return fmt.Sprintf(
			"Your ecosystem has %d ISP(s) in critical condition requiring immediate attention. "+
				"Overall health score is %.0f/100. Focus on the critical ISPs first — "+
				"each has a tailored activation strategy with phased warmup schedules. "+
				"Total sending volume: %s messages analyzed.",
			criticalCount, snapshot.OverallHealth, formatVolume(snapshot.TotalSendingVolume))
	}

	if highCount > 0 {
		return fmt.Sprintf(
			"Your ecosystem has %d ISP(s) with elevated risk levels. "+
				"Overall health score is %.0f/100. The recommended strategies will help "+
				"bring these ISPs back to healthy thresholds within 2-4 weeks. "+
				"%d ISP(s) are already performing well.",
			highCount, snapshot.OverallHealth, healthyCount)
	}

	return fmt.Sprintf(
		"Your ecosystem is performing at %.0f/100 overall health. "+
			"%d ISP(s) are in healthy condition. Continue current practices "+
			"and consider the optimization strategies to further improve performance.",
		snapshot.OverallHealth, healthyCount)
}

// ============================================================================
// Helper functions
// ============================================================================

func computeBounceImpact(bounceRate, hardBounceRate, maxBounceRate float64) float64 {
	if bounceRate <= 0 {
		return 30.0
	}
	bounceRatio := bounceRate / maxBounceRate
	if bounceRatio >= 3.0 {
		return 0.0
	}
	if bounceRatio <= 0.5 {
		return 30.0
	}
	score := 30.0 * (1.0 - (bounceRatio-0.5)/2.5)
	if hardBounceRate > maxBounceRate*0.5 {
		penalty := (hardBounceRate - maxBounceRate*0.5) / maxBounceRate * 10.0
		score -= penalty
	}
	return math.Max(0, math.Min(30, score))
}

func computeComplaintImpact(complaintRate, spamTrapRate, maxComplaintRate, maxSpamTrapRate float64) float64 {
	if complaintRate <= 0 && spamTrapRate <= 0 {
		return 35.0
	}
	score := 35.0
	complaintRatio := complaintRate / maxComplaintRate
	if maxComplaintRate > 0 {
		if complaintRatio >= 3.0 {
			score -= 25.0
		} else if complaintRatio > 0.5 {
			score -= 25.0 * (complaintRatio - 0.5) / 2.5
		}
	}
	if maxSpamTrapRate > 0 {
		spamTrapRatio := spamTrapRate / maxSpamTrapRate
		if spamTrapRatio >= 2.0 {
			score -= 10.0
		} else if spamTrapRatio > 0.5 {
			score -= 10.0 * (spamTrapRatio - 0.5) / 1.5
		}
	}
	return math.Max(0, math.Min(35, score))
}

func computeEngagementImpact(engagementRate, clickToOpenRate, minEngagementRate float64) float64 {
	if minEngagementRate <= 0 {
		return 35.0
	}
	var score float64
	engagementRatio := engagementRate / minEngagementRate
	if engagementRatio >= 2.0 {
		score += 25.0
	} else if engagementRatio >= 1.0 {
		score += 15.0 + 10.0*(engagementRatio-1.0)
	} else if engagementRatio > 0 {
		score += 15.0 * engagementRatio
	}
	if clickToOpenRate >= 20.0 {
		score += 10.0
	} else if clickToOpenRate >= 10.0 {
		score += 5.0 + 5.0*(clickToOpenRate-10.0)/10.0
	} else if clickToOpenRate > 0 {
		score += 5.0 * (clickToOpenRate / 10.0)
	}
	return math.Max(0, math.Min(35, score))
}

func computeRiskLevel(overallScore float64) RiskLevel {
	switch {
	case overallScore >= 80:
		return RiskHealthy
	case overallScore >= 65:
		return RiskLow
	case overallScore >= 45:
		return RiskMedium
	case overallScore >= 25:
		return RiskHigh
	default:
		return RiskCritical
	}
}

func (dai *DataActivationIntelligence) generateDiagnostics(isp ISP, score DataHealthScore, thresholds ISPThresholds) []string {
	var diags []string
	name := ispDisplayName(isp)

	if score.BounceRate > thresholds.MaxBounceRate {
		diags = append(diags, fmt.Sprintf("%s bounce rate %.2f%% exceeds threshold %.2f%% — list hygiene needed", name, score.BounceRate, thresholds.MaxBounceRate))
	}
	if score.HardBounceRate > thresholds.MaxBounceRate*0.5 {
		diags = append(diags, fmt.Sprintf("%s hard bounce rate %.2f%% is elevated — indicates stale or invalid addresses", name, score.HardBounceRate))
	}
	if score.ComplaintRate > thresholds.MaxComplaintRate {
		diags = append(diags, fmt.Sprintf("%s complaint rate %.4f%% exceeds threshold %.4f%% — immediate action required", name, score.ComplaintRate, thresholds.MaxComplaintRate))
	}
	if score.EngagementRate < thresholds.MinEngagementRate {
		diags = append(diags, fmt.Sprintf("%s engagement rate %.2f%% below minimum %.2f%% — re-engagement campaigns needed", name, score.EngagementRate, thresholds.MinEngagementRate))
	}
	if score.SpamTrapRate > thresholds.MaxSpamTrapRate {
		diags = append(diags, fmt.Sprintf("%s spam trap hit rate %.4f%% exceeds threshold — list source audit needed", name, score.SpamTrapRate))
	}
	if score.DeliveryRate > 0 && score.DeliveryRate < 95.0 {
		diags = append(diags, fmt.Sprintf("%s delivery rate %.2f%% is below 95%% — deliverability issues detected", name, score.DeliveryRate))
	}
	if score.TotalVolume < 1000 && score.TotalVolume > 0 {
		diags = append(diags, fmt.Sprintf("%s sending volume (%d) is low — warmup may be required before scaling", name, score.TotalVolume))
	}

	if len(diags) == 0 {
		diags = append(diags, fmt.Sprintf("%s metrics are within healthy thresholds — maintain current practices", name))
	}

	return diags
}

func (dai *DataActivationIntelligence) getThresholds(isp ISP) ISPThresholds {
	if t, ok := dai.ispThresholds[isp]; ok {
		return t
	}
	return dai.ispThresholds[ISPOther]
}

func safePercent(numerator, denominator float64) float64 {
	if denominator == 0 {
		return 0
	}
	return math.Round(numerator/denominator*10000) / 100
}

func ispDisplayName(isp ISP) string {
	switch isp {
	case ISPGmail:
		return "Gmail"
	case ISPYahoo:
		return "Yahoo Mail"
	case ISPOutlook:
		return "Microsoft Outlook"
	case ISPAOL:
		return "AOL Mail"
	case ISPApple:
		return "Apple Mail"
	default:
		return "Other ISPs"
	}
}

func formatVolume(v int64) string {
	if v >= 1000000 {
		return fmt.Sprintf("%.1fM", float64(v)/1000000)
	}
	if v >= 1000 {
		return fmt.Sprintf("%.1fK", float64(v)/1000)
	}
	return fmt.Sprintf("%d", v)
}
