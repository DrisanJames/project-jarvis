package activation

import "fmt"

// getISPStrategyDescription returns a rich, multi-sentence strategy description
// tailored to each ISP's unique filtering model and deliverability requirements.
// Built by Jarvis (Opus 4.6).
func getISPStrategyDescription(isp ISP, stratType StrategyType, score DataHealthScore) string {
	switch isp {
	case ISPGmail:
		return getGmailStrategy(stratType, score)
	case ISPYahoo:
		return getYahooStrategy(stratType, score)
	case ISPOutlook:
		return getOutlookStrategy(stratType, score)
	case ISPAOL:
		return getAOLStrategy(stratType, score)
	case ISPApple:
		return getAppleStrategy(stratType, score)
	default:
		return getDefaultStrategy(stratType, score)
	}
}

func getGmailStrategy(stratType StrategyType, score DataHealthScore) string {
	base := "Gmail prioritises recipient engagement above all other signals. " +
		"Their ML-based filtering learns from user behaviour in real-time — " +
		"if subscribers don't open or click, future messages route to Promotions or Spam. "

	switch stratType {
	case StrategyWarmup:
		return base + "For domain warmup, start exclusively with 7-day openers. Send 500-1,000 emails per day " +
			"for the first week, doubling weekly only if bounce rate stays below 2% and opens exceed 20%. " +
			"Gmail's algorithms need consistent positive engagement signals over 2-4 weeks before trusting new senders."
	case StrategyEngagementRamp:
		return base + fmt.Sprintf("Current engagement rate is %.2f%% — below Gmail's expected threshold. "+
			"Reduce your active sending list to ONLY 7-day openers. Send compelling, personalised content "+
			"with clear CTAs. Consider AMP for Email to boost interactivity. "+
			"Once engagement climbs above 15%%, gradually expand to 14-day openers.", score.EngagementRate)
	case StrategyRepRepair:
		return base + "Your Gmail reputation needs repair. Immediately reduce volume by 50% and restrict to " +
			"7-day openers only. Run a re-engagement campaign to identify truly active subscribers. " +
			"Remove anyone who hasn't engaged in 30 days. Rebuild trust over 3-4 weeks with high-quality, " +
			"highly-targeted sends."
	case StrategyComplaintControl:
		return base + fmt.Sprintf("Gmail complaint rate is %.4f%% — while Gmail uses engagement more than "+
			"complaints, high spam reports accelerate spam folder placement. "+
			"Add a prominent unsubscribe header, reduce frequency, and only send to recent openers.",
			score.ComplaintRate)
	default:
		return base + "Continue sending to engaged segments. Monitor Postmaster Tools for domain " +
			"reputation changes. Scale volume gradually (20% per week) while maintaining engagement above 15%."
	}
}

func getYahooStrategy(stratType StrategyType, score DataHealthScore) string {
	base := "Yahoo (and AOL, which shares its infrastructure) weights complaint rate " +
		"more heavily than any other ISP. Their filtering is complaint-rate driven with " +
		"sender-reputation scoring. "

	switch stratType {
	case StrategyWarmup:
		return base + "For Yahoo warmup, register for the Yahoo Complaint Feedback Loop (CFL) BEFORE " +
			"sending any volume. Start with 300-500 emails/day to your most engaged clickers (not just openers). " +
			"Yahoo recycling spam traps are aggressive — ensure every address has been recently validated. " +
			"Include a visible unsubscribe link in the first viewport of every email."
	case StrategyComplaintControl:
		return base + fmt.Sprintf("Yahoo complaint rate is %.4f%% — this exceeds the safe threshold. "+
			"Immediately: 1) Reduce volume by 60%%, 2) Send only to 14-day clickers, "+
			"3) Add unsubscribe link to email header AND body, 4) Process CFL removals in real-time. "+
			"Yahoo will throttle or block senders who exceed 0.05%% complaint rate.",
			score.ComplaintRate)
	case StrategyListHygiene:
		return base + fmt.Sprintf("Yahoo bounce rate is %.2f%% — indicating stale list data. "+
			"Run email validation on all Yahoo addresses. Remove any that haven't engaged in 90 days — "+
			"Yahoo recycles abandoned accounts as spam traps. "+
			"After cleaning, re-warmup with a conservative volume schedule.",
			score.BounceRate)
	case StrategyEngagementRamp:
		return base + "Segment Yahoo subscribers by click recency, not opens. Send during mid-morning " +
			"hours (9-11 AM local time) when Yahoo users are most active. Use full branded URLs — " +
			"Yahoo flags link shorteners. Personalise content with dynamic elements to increase clicks."
	default:
		return base + "Maintain complaint rate below 0.05%. Continue CFL processing. " +
			"Scale conservatively (15% per week) with click-based segmentation."
	}
}

func getOutlookStrategy(stratType StrategyType, score DataHealthScore) string {
	base := "Microsoft Outlook/Hotmail uses SmartScreen filtering combined with domain " +
		"authentication scoring. They heavily weight SPF, DKIM, and DMARC alignment. "

	switch stratType {
	case StrategyWarmup:
		return base + "Before warming up on Outlook, ensure DMARC is set to p=reject with full alignment. " +
			"Apply to Microsoft SNDS and JMRP programmes. Start with 200-500 emails/day to 30-day engaged " +
			"subscribers. Outlook is more tolerant of volume than Gmail, but less forgiving of " +
			"authentication failures. Increase volume by 25% every 3 days."
	case StrategyAuthFocus:
		return base + "Your Outlook deliverability is suffering from authentication issues. " +
			"Verify: 1) SPF record includes all sending IPs, 2) DKIM signatures are valid, " +
			"3) DMARC policy is p=reject with alignment, 4) SNDS shows clean reputation. " +
			"Fix authentication before increasing volume."
	case StrategyEngagementRamp:
		return base + fmt.Sprintf("Outlook engagement rate is %.2f%%. "+
			"Personalise subject lines with recipient's first name — SmartScreen rewards "+
			"one-to-one patterns. Keep HTML clean and minimal. "+
			"Send during business hours (8-10 AM local) when Outlook users check work email.",
			score.EngagementRate)
	default:
		return base + "Maintain DMARC alignment and clean SNDS reputation. " +
			"Scale with 30-day engaged audience. Monitor Junk folder placement via seed testing."
	}
}

func getAOLStrategy(stratType StrategyType, score DataHealthScore) string {
	base := "AOL shares Yahoo's filtering infrastructure, making complaint-rate management " +
		"the top priority. AOL's user base tends to be older and less tech-savvy. "

	switch stratType {
	case StrategyWarmup:
		return base + "Start with 200-300 emails/day to AOL subscribers who clicked within the last 14 days. " +
			"Use simple, single-column layouts with large fonts (16px+) and high-contrast CTA buttons. " +
			"AOL recycles abandoned accounts as spam traps — validate all addresses before sending."
	case StrategyComplaintControl:
		return base + fmt.Sprintf("AOL complaint rate is %.4f%% — since AOL uses Yahoo's infrastructure, "+
			"this directly impacts your Yahoo reputation too. Reduce AOL volume immediately, "+
			"restrict to recent clickers only, and ensure every email has a prominent unsubscribe option.",
			score.ComplaintRate)
	case StrategyListHygiene:
		return base + "Re-confirm inactive AOL addresses every 90 days. Remove any address that " +
			"hasn't engaged in 60 days. AOL's spam trap network is aggressive, and a single " +
			"trap hit can damage reputation across both AOL and Yahoo."
	default:
		return base + "Target evening sends (6-9 PM EST) for better engagement. " +
			"Keep designs accessible with large text and clear, simple CTAs."
	}
}

func getAppleStrategy(stratType StrategyType, score DataHealthScore) string {
	base := "Apple Mail Privacy Protection (MPP) pre-fetches images via relay servers, " +
		"inflating open rates and masking true engagement. Apple enforces strict authentication. "

	switch stratType {
	case StrategyWarmup:
		return base + "For Apple Mail warmup, focus on click metrics rather than opens (MPP makes " +
			"opens unreliable). Start with 500-1,000 emails/day to known clickers. Ensure " +
			"SPF/DKIM/DMARC are fully aligned — Apple is strict on authentication. " +
			"Test both light and dark mode rendering before scaling."
	case StrategyEngagementRamp:
		return base + "Since open rates are inflated by MPP, measure success by click-through rate, " +
			"conversion rate, and reply rate. Build re-engagement flows based on clicks, not opens. " +
			"Send during lunch hours (12-2 PM local) when Apple users browse on mobile."
	default:
		return base + "Maintain strict authentication. Use click-based segmentation exclusively. " +
			"Monitor conversion metrics rather than vanity open rates."
	}
}

func getDefaultStrategy(stratType StrategyType, score DataHealthScore) string {
	return "Follow standard deliverability best practices: authenticate with SPF/DKIM/DMARC, " +
		"monitor bounce rates, and keep complaint rate below 0.1%. " +
		"Segment by engagement recency and scale volume gradually."
}

// getISPKeyActions returns ordered action items for each ISP strategy
func getISPKeyActions(isp ISP, stratType StrategyType, score DataHealthScore) []string {
	switch isp {
	case ISPGmail:
		return []string{
			"Restrict sending to 7-day openers only for initial phase",
			"Monitor Google Postmaster Tools daily for reputation changes",
			"A/B test subject lines to maximize open rates (target > 20%)",
			"Implement AMP for Email for interactive content where possible",
			"Gradually expand to 14-day openers once engagement stabilizes above 15%",
			"Add list-unsubscribe header for one-click unsubscribe",
		}
	case ISPYahoo:
		return []string{
			"Register for Yahoo Complaint Feedback Loop (CFL) immediately",
			"Process CFL removals in real-time (< 1 hour)",
			"Place unsubscribe link prominently in first viewport",
			"Send only to 14-day clickers (click-based, not open-based)",
			"Use full branded URLs — avoid link shorteners",
			"Schedule sends 9-11 AM recipient local time",
			"Validate all Yahoo addresses for spam trap risk",
		}
	case ISPOutlook:
		return []string{
			"Verify DMARC alignment is p=reject",
			"Apply to Microsoft SNDS and JMRP",
			"Personalise subject lines with first name",
			"Keep HTML clean — no CSS floats, no JavaScript",
			"Target 30-day engaged audience for initial sends",
			"Monitor SmartScreen verdicts via SNDS dashboard",
		}
	case ISPAOL:
		return []string{
			"Use 16px+ fonts and high-contrast CTA buttons",
			"Single-column layout for AOL webmail compatibility",
			"Keep total image size under 100KB",
			"Re-confirm inactive addresses every 90 days",
			"Schedule evening sends (6-9 PM EST)",
			"Process Yahoo CFL removals (shared infrastructure)",
		}
	case ISPApple:
		return []string{
			"Switch to click-based engagement metrics (ignore open rates)",
			"Ensure SPF/DKIM/DMARC fully aligned",
			"Test dark mode rendering before sending",
			"Build re-engagement flows based on clicks and conversions",
			"Schedule lunch-hour sends (12-2 PM local time)",
			"Use link-decoration to identify Apple Mail clients",
		}
	default:
		return []string{
			"Authenticate with SPF/DKIM/DMARC",
			"Monitor bounce and complaint rates",
			"Segment by engagement recency",
			"Scale volume gradually (20% per week)",
		}
	}
}

// generateWarmupSchedule creates a phased volume ramp for the ISP
func generateWarmupSchedule(isp ISP, stratType StrategyType, score DataHealthScore) []WarmupPhase {
	var phases []WarmupPhase

	switch isp {
	case ISPGmail:
		phases = []WarmupPhase{
			{PhaseNumber: 1, DurationDays: 7, DailyVolume: 500, VolumePercent: 5, TargetSegment: "7-day openers", MaxBounceRate: 1.5, MaxComplaintRate: 0.05, MinEngagement: 20.0, Actions: []string{"Send to most engaged Gmail subscribers only", "Monitor Postmaster Tools daily"}, GatingCriteria: []string{"Bounce rate < 1.5%", "Open rate > 20%", "No spam folder placement"}},
			{PhaseNumber: 2, DurationDays: 7, DailyVolume: 2000, VolumePercent: 15, TargetSegment: "14-day openers", MaxBounceRate: 2.0, MaxComplaintRate: 0.06, MinEngagement: 18.0, Actions: []string{"Expand to 14-day openers", "A/B test subject lines"}, GatingCriteria: []string{"Phase 1 metrics maintained", "Engagement rate > 18%"}},
			{PhaseNumber: 3, DurationDays: 7, DailyVolume: 5000, VolumePercent: 40, TargetSegment: "30-day engaged", MaxBounceRate: 2.0, MaxComplaintRate: 0.08, MinEngagement: 15.0, Actions: []string{"Expand to 30-day engaged", "Introduce offer variety"}, GatingCriteria: []string{"Consistent inbox placement", "Engagement rate > 15%"}},
			{PhaseNumber: 4, DurationDays: 7, DailyVolume: 10000, VolumePercent: 100, TargetSegment: "Full engaged audience", MaxBounceRate: 2.0, MaxComplaintRate: 0.08, MinEngagement: 15.0, Actions: []string{"Full volume to engaged audience", "Optimise for conversion"}, GatingCriteria: []string{"All metrics within healthy thresholds"}},
		}
	case ISPYahoo:
		phases = []WarmupPhase{
			{PhaseNumber: 1, DurationDays: 5, DailyVolume: 300, VolumePercent: 5, TargetSegment: "14-day clickers", MaxBounceRate: 2.0, MaxComplaintRate: 0.03, MinEngagement: 12.0, Actions: []string{"CFL registration verified", "Click-based segmentation only"}, GatingCriteria: []string{"Complaint rate < 0.03%", "Bounce rate < 2%"}},
			{PhaseNumber: 2, DurationDays: 7, DailyVolume: 1500, VolumePercent: 20, TargetSegment: "30-day clickers", MaxBounceRate: 2.5, MaxComplaintRate: 0.04, MinEngagement: 10.0, Actions: []string{"Expand to 30-day clickers", "Monitor CFL closely"}, GatingCriteria: []string{"Complaint rate < 0.04%", "No blocking events"}},
			{PhaseNumber: 3, DurationDays: 7, DailyVolume: 4000, VolumePercent: 60, TargetSegment: "60-day engaged", MaxBounceRate: 3.0, MaxComplaintRate: 0.05, MinEngagement: 8.0, Actions: []string{"Expand carefully to 60-day engaged", "Validate addresses"}, GatingCriteria: []string{"Complaint rate < 0.05%", "Delivery rate > 95%"}},
			{PhaseNumber: 4, DurationDays: 7, DailyVolume: 8000, VolumePercent: 100, TargetSegment: "Full engaged audience", MaxBounceRate: 3.0, MaxComplaintRate: 0.05, MinEngagement: 8.0, Actions: []string{"Full volume with complaint monitoring"}, GatingCriteria: []string{"All Yahoo thresholds met"}},
		}
	default:
		phases = []WarmupPhase{
			{PhaseNumber: 1, DurationDays: 7, DailyVolume: 500, VolumePercent: 10, TargetSegment: "Most engaged", MaxBounceRate: 2.0, MaxComplaintRate: 0.05, MinEngagement: 15.0, Actions: []string{"Start with most engaged segment"}, GatingCriteria: []string{"Clean initial metrics"}},
			{PhaseNumber: 2, DurationDays: 7, DailyVolume: 2500, VolumePercent: 35, TargetSegment: "30-day engaged", MaxBounceRate: 2.5, MaxComplaintRate: 0.08, MinEngagement: 12.0, Actions: []string{"Expand audience gradually"}, GatingCriteria: []string{"Phase 1 metrics maintained"}},
			{PhaseNumber: 3, DurationDays: 7, DailyVolume: 5000, VolumePercent: 70, TargetSegment: "60-day engaged", MaxBounceRate: 3.0, MaxComplaintRate: 0.1, MinEngagement: 10.0, Actions: []string{"Scale to target volume"}, GatingCriteria: []string{"Delivery rate > 95%"}},
			{PhaseNumber: 4, DurationDays: 7, DailyVolume: 10000, VolumePercent: 100, TargetSegment: "Full audience", MaxBounceRate: 3.0, MaxComplaintRate: 0.1, MinEngagement: 8.0, Actions: []string{"Full volume operations"}, GatingCriteria: []string{"All thresholds met"}},
		}
	}

	return phases
}

// getISPRisks returns potential risks for each ISP strategy
func getISPRisks(isp ISP, stratType StrategyType) []string {
	switch isp {
	case ISPGmail:
		return []string{
			"Sending to non-openers will rapidly damage domain reputation",
			"Promotions tab placement reduces visibility — not necessarily deliverability",
			"Volume scaling too fast triggers automated throttling",
		}
	case ISPYahoo:
		return []string{
			"Complaint rate spikes cause immediate blocking (< 24 hours)",
			"Yahoo spam traps are aggressively recycled from abandoned accounts",
			"AOL reputation is tied to Yahoo — damage affects both",
		}
	case ISPOutlook:
		return []string{
			"SmartScreen changes can suddenly increase Junk folder placement",
			"DMARC misalignment results in immediate filtering",
			"SNDS may not reflect real-time changes for 24-48 hours",
		}
	case ISPAOL:
		return []string{
			"Shared Yahoo infrastructure means AOL issues affect Yahoo and vice versa",
			"Older user base may generate more complaints from unfamiliar senders",
			"Spam trap recycling is more aggressive in the AOL namespace",
		}
	case ISPApple:
		return []string{
			"Mail Privacy Protection makes engagement measurement unreliable",
			"Strict authentication enforcement can cause sudden delivery drops",
			"Dark mode rendering issues can make emails unreadable",
		}
	default:
		return []string{
			"Unknown ISP filtering may change without notice",
			"Limited feedback loop availability",
		}
	}
}

// getISPSuccessMetrics returns what to measure for success
func getISPSuccessMetrics(isp ISP) []string {
	switch isp {
	case ISPGmail:
		return []string{
			"Google Postmaster Tools domain reputation: High",
			"Engagement rate > 15%",
			"Primary inbox placement > 80% (via seed testing)",
			"Bounce rate < 2%",
		}
	case ISPYahoo:
		return []string{
			"Complaint rate < 0.05%",
			"CFL processing time < 1 hour",
			"Delivery rate > 97%",
			"Click-through rate trending upward",
		}
	case ISPOutlook:
		return []string{
			"SNDS status: Green",
			"DMARC alignment: 100%",
			"Junk folder placement < 5%",
			"SmartScreen pass rate > 95%",
		}
	case ISPAOL:
		return []string{
			"Complaint rate < 0.05% (shared with Yahoo)",
			"No spam trap hits",
			"Evening engagement rate > 8%",
			"Zero blocking events",
		}
	case ISPApple:
		return []string{
			"Click-through rate > 3%",
			"Conversion rate trending upward",
			"Authentication pass rate: 100%",
			"Reply rate > 0.5%",
		}
	default:
		return []string{
			"Bounce rate < 3%",
			"Complaint rate < 0.1%",
			"Delivery rate > 95%",
		}
	}
}
