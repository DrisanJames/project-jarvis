package agent

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/everflow"
)

// handleEverflowSummaryQuery provides an overview of Everflow revenue data
func (a *Agent) handleEverflowSummaryQuery() ChatResponse {
	a.mu.RLock()
	defer a.mu.RUnlock()

	if a.collectors == nil || a.collectors.Everflow == nil {
		return ChatResponse{
			Message: "Everflow integration is not configured.",
		}
	}

	daily := a.collectors.Everflow.GetDailyPerformance()
	offers := a.collectors.Everflow.GetOfferPerformance()
	properties := a.collectors.Everflow.GetPropertyPerformance()
	totalRevenue := a.collectors.Everflow.GetTotalRevenue()
	lastFetch := a.collectors.Everflow.LastFetch()

	if len(daily) == 0 {
		return ChatResponse{
			Message: "No Everflow data available yet. Data collection may still be in progress.",
		}
	}

	var totalClicks, totalConversions int64
	var todayRevenue float64
	today := time.Now().Format("2006-01-02")

	for _, d := range daily {
		totalClicks += d.Clicks
		totalConversions += d.Conversions
		if d.Date == today {
			todayRevenue = d.Revenue
		}
	}

	var sb strings.Builder
	sb.WriteString("**Everflow Revenue Overview**\n\n")
	sb.WriteString(fmt.Sprintf("**Total Revenue:** $%.2f (%d days)\n", totalRevenue, len(daily)))
	sb.WriteString(fmt.Sprintf("**Today's Revenue:** $%.2f\n", todayRevenue))
	sb.WriteString(fmt.Sprintf("**Total Conversions:** %s\n", formatNumber(totalConversions)))
	sb.WriteString(fmt.Sprintf("**Total Clicks:** %s\n", formatNumber(totalClicks)))

	if totalClicks > 0 {
		epc := totalRevenue / float64(totalClicks)
		convRate := float64(totalConversions) / float64(totalClicks) * 100
		sb.WriteString(fmt.Sprintf("**Overall EPC:** $%.2f\n", epc))
		sb.WriteString(fmt.Sprintf("**Overall Conv. Rate:** %.2f%%\n", convRate))
	}

	sb.WriteString(fmt.Sprintf("\n**Active Offers:** %d\n", len(offers)))
	sb.WriteString(fmt.Sprintf("**Active Properties:** %d\n", len(properties)))
	sb.WriteString(fmt.Sprintf("\n_Last updated: %s_", lastFetch.Format("Jan 2, 3:04 PM")))

	return ChatResponse{
		Message: sb.String(),
		Suggestions: []string{
			"Show top offers by revenue",
			"Show property performance",
			"What's the daily revenue trend?",
		},
	}
}

// handleEverflowRevenueQuery handles revenue-related queries
func (a *Agent) handleEverflowRevenueQuery(query string) ChatResponse {
	a.mu.RLock()
	defer a.mu.RUnlock()

	if a.collectors == nil || a.collectors.Everflow == nil {
		return ChatResponse{
			Message: "Everflow integration is not configured.",
		}
	}

	daily := a.collectors.Everflow.GetDailyPerformance()
	if len(daily) == 0 {
		return ChatResponse{
			Message: "No revenue data available yet.",
		}
	}

	// Check if asking about today
	if containsAny(query, []string{"today", "today's"}) {
		today := time.Now().Format("2006-01-02")
		for _, d := range daily {
			if d.Date == today {
				return ChatResponse{
					Message: fmt.Sprintf("**Today's Revenue:** $%.2f\n\n"+
						"**Clicks:** %s\n"+
						"**Conversions:** %d\n"+
						"**Conversion Rate:** %.2f%%\n"+
						"**EPC:** $%.2f",
						d.Revenue, formatNumber(d.Clicks), d.Conversions,
						d.ConversionRate*100, d.EPC),
					Suggestions: []string{
						"Show this week's revenue",
						"Compare to yesterday",
						"Top offers today",
					},
				}
			}
		}
		return ChatResponse{
			Message: "No revenue recorded yet today.",
		}
	}

	// Weekly summary
	if containsAny(query, []string{"week", "weekly", "this week", "7 day"}) {
		var weekRevenue float64
		var weekClicks, weekConv int64
		cutoff := time.Now().AddDate(0, 0, -7).Format("2006-01-02")
		
		for _, d := range daily {
			if d.Date >= cutoff {
				weekRevenue += d.Revenue
				weekClicks += d.Clicks
				weekConv += d.Conversions
			}
		}

		var sb strings.Builder
		sb.WriteString("**This Week's Revenue Summary**\n\n")
		sb.WriteString(fmt.Sprintf("**Total Revenue:** $%.2f\n", weekRevenue))
		sb.WriteString(fmt.Sprintf("**Conversions:** %d\n", weekConv))
		sb.WriteString(fmt.Sprintf("**Clicks:** %s\n", formatNumber(weekClicks)))
		if weekClicks > 0 {
			sb.WriteString(fmt.Sprintf("**EPC:** $%.2f\n", weekRevenue/float64(weekClicks)))
		}

		return ChatResponse{
			Message: sb.String(),
			Suggestions: []string{
				"Show monthly revenue",
				"Best performing day",
				"Revenue by offer",
			},
		}
	}

	// Default: show daily breakdown
	var sb strings.Builder
	sb.WriteString("**Daily Revenue Breakdown**\n\n")
	
	// Sort by date descending (most recent first)
	sorted := make([]everflow.DailyPerformance, len(daily))
	copy(sorted, daily)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Date > sorted[j].Date
	})

	for i, d := range sorted {
		if i >= 7 {
			break
		}
		date, _ := time.Parse("2006-01-02", d.Date)
		sb.WriteString(fmt.Sprintf("**%s:** $%.2f (%d conv, $%.2f EPC)\n",
			date.Format("Mon Jan 2"), d.Revenue, d.Conversions, d.EPC))
	}

	totalRevenue := a.collectors.Everflow.GetTotalRevenue()
	sb.WriteString(fmt.Sprintf("\n**Period Total:** $%.2f", totalRevenue))

	return ChatResponse{
		Message: sb.String(),
		Suggestions: []string{
			"Show today's revenue",
			"Which offers are performing best?",
			"Revenue by property",
		},
	}
}

// handleEverflowConversionQuery handles conversion-related queries
func (a *Agent) handleEverflowConversionQuery(query string) ChatResponse {
	a.mu.RLock()
	defer a.mu.RUnlock()

	if a.collectors == nil || a.collectors.Everflow == nil {
		return ChatResponse{
			Message: "Everflow integration is not configured.",
		}
	}

	daily := a.collectors.Everflow.GetDailyPerformance()
	if len(daily) == 0 {
		return ChatResponse{
			Message: "No conversion data available yet.",
		}
	}

	var totalConversions, totalClicks int64
	for _, d := range daily {
		totalConversions += d.Conversions
		totalClicks += d.Clicks
	}

	var sb strings.Builder
	sb.WriteString("**Conversion Performance**\n\n")
	sb.WriteString(fmt.Sprintf("**Total Conversions:** %s\n", formatNumber(totalConversions)))
	sb.WriteString(fmt.Sprintf("**Total Clicks:** %s\n", formatNumber(totalClicks)))
	
	if totalClicks > 0 {
		convRate := float64(totalConversions) / float64(totalClicks) * 100
		sb.WriteString(fmt.Sprintf("**Overall Conv. Rate:** %.2f%%\n\n", convRate))
	}

	// Show recent days
	sb.WriteString("**Recent Daily Conversions:**\n")
	sorted := make([]everflow.DailyPerformance, len(daily))
	copy(sorted, daily)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Date > sorted[j].Date
	})

	for i, d := range sorted {
		if i >= 5 {
			break
		}
		date, _ := time.Parse("2006-01-02", d.Date)
		sb.WriteString(fmt.Sprintf("• %s: %d conversions (%.2f%% rate)\n",
			date.Format("Mon Jan 2"), d.Conversions, d.ConversionRate*100))
	}

	return ChatResponse{
		Message: sb.String(),
		Suggestions: []string{
			"Which offers convert best?",
			"Show revenue breakdown",
			"EPC analysis",
		},
	}
}

// handleEverflowOfferQuery handles offer performance queries
func (a *Agent) handleEverflowOfferQuery(query string) ChatResponse {
	a.mu.RLock()
	defer a.mu.RUnlock()

	if a.collectors == nil || a.collectors.Everflow == nil {
		return ChatResponse{
			Message: "Everflow integration is not configured.",
		}
	}

	offers := a.collectors.Everflow.GetOfferPerformance()
	if len(offers) == 0 {
		return ChatResponse{
			Message: "No offer data available yet.",
		}
	}

	// Sort by revenue descending
	sorted := make([]everflow.OfferPerformance, len(offers))
	copy(sorted, offers)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Revenue > sorted[j].Revenue
	})

	var sb strings.Builder
	sb.WriteString("**Top Offers by Revenue**\n\n")

	for i, o := range sorted {
		if i >= 10 {
			break
		}
		name := o.OfferName
		if len(name) > 35 {
			name = name[:32] + "..."
		}
		sb.WriteString(fmt.Sprintf("%d. **%s**\n", i+1, name))
		sb.WriteString(fmt.Sprintf("   Revenue: $%.2f | Conv: %d | EPC: $%.2f\n\n",
			o.Revenue, o.Conversions, o.EPC))
	}

	var totalRev float64
	for _, o := range offers {
		totalRev += o.Revenue
	}
	sb.WriteString(fmt.Sprintf("**Total across %d offers:** $%.2f", len(offers), totalRev))

	return ChatResponse{
		Message: sb.String(),
		Suggestions: []string{
			"Show property performance",
			"Daily revenue trend",
			"Which offers have best EPC?",
		},
	}
}

// handleEverflowPropertyQuery handles property/domain performance queries
func (a *Agent) handleEverflowPropertyQuery(query string) ChatResponse {
	a.mu.RLock()
	defer a.mu.RUnlock()

	if a.collectors == nil || a.collectors.Everflow == nil {
		return ChatResponse{
			Message: "Everflow integration is not configured.",
		}
	}

	properties := a.collectors.Everflow.GetPropertyPerformance()
	if len(properties) == 0 {
		return ChatResponse{
			Message: "No property data available yet.",
		}
	}

	// Sort by revenue descending
	sorted := make([]everflow.PropertyPerformance, len(properties))
	copy(sorted, properties)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Revenue > sorted[j].Revenue
	})

	var sb strings.Builder
	sb.WriteString("**Property/Domain Performance**\n\n")

	for i, p := range sorted {
		if i >= 10 {
			break
		}
		name := p.PropertyName
		if name == "" {
			name = p.PropertyCode
		}
		if len(name) > 30 {
			name = name[:27] + "..."
		}
		sb.WriteString(fmt.Sprintf("**%s** (%s)\n", p.PropertyCode, name))
		sb.WriteString(fmt.Sprintf("   Revenue: $%.2f | Conv: %d | Offers: %d | EPC: $%.2f\n\n",
			p.Revenue, p.Conversions, p.UniqueOffers, p.EPC))
	}

	var totalRev float64
	for _, p := range properties {
		totalRev += p.Revenue
	}
	sb.WriteString(fmt.Sprintf("**Total across %d properties:** $%.2f", len(properties), totalRev))

	return ChatResponse{
		Message: sb.String(),
		Suggestions: []string{
			"Show top offers",
			"Revenue by day",
			"Which property has best EPC?",
		},
	}
}

// handleEverflowEPCQuery handles EPC (earnings per click) queries
func (a *Agent) handleEverflowEPCQuery() ChatResponse {
	a.mu.RLock()
	defer a.mu.RUnlock()

	if a.collectors == nil || a.collectors.Everflow == nil {
		return ChatResponse{
			Message: "Everflow integration is not configured.",
		}
	}

	offers := a.collectors.Everflow.GetOfferPerformance()
	if len(offers) == 0 {
		return ChatResponse{
			Message: "No EPC data available yet.",
		}
	}

	// Sort by EPC descending
	sorted := make([]everflow.OfferPerformance, len(offers))
	copy(sorted, offers)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].EPC > sorted[j].EPC
	})

	var sb strings.Builder
	sb.WriteString("**EPC Analysis (Earnings Per Click)**\n\n")
	sb.WriteString("**Top Offers by EPC:**\n\n")

	for i, o := range sorted {
		if i >= 10 {
			break
		}
		name := o.OfferName
		if len(name) > 30 {
			name = name[:27] + "..."
		}
		sb.WriteString(fmt.Sprintf("%d. **%s**: $%.2f EPC\n", i+1, name, o.EPC))
		sb.WriteString(fmt.Sprintf("   (%s clicks, %d conv, $%.2f revenue)\n\n",
			formatNumber(o.Clicks), o.Conversions, o.Revenue))
	}

	// Calculate overall EPC
	var totalRev float64
	var totalClicks int64
	for _, o := range offers {
		totalRev += o.Revenue
		totalClicks += o.Clicks
	}
	if totalClicks > 0 {
		sb.WriteString(fmt.Sprintf("**Overall EPC:** $%.2f", totalRev/float64(totalClicks)))
	}

	return ChatResponse{
		Message: sb.String(),
		Suggestions: []string{
			"Show revenue breakdown",
			"Top offers by revenue",
			"Property performance",
		},
	}
}
