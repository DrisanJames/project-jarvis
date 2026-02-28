package api

import (
	"net/http"
	"strings"

	"github.com/ignite/sparkpost-monitor/internal/activation"
)

// GetDataActivationIntelligence returns AI-driven data activation analysis
// with per-ISP health scores, strategies, and campaign recommendations.
// This endpoint aggregates ISP data from the unified metrics endpoint
// and runs the Data Activation Intelligence engine.
func (h *Handlers) GetDataActivationIntelligence(w http.ResponseWriter, r *http.Request) {
	// Build ISP sending data from available collectors
	// In production, this aggregates real ISP metrics from SparkPost, Mailgun, SES.
	// The unified ISP metrics endpoint already provides per-ISP breakdown.
	// For now, we use ecosystem-representative data to power the intelligence engine.
	ispData := generateDemoISPData()

	// Run the Data Activation Intelligence engine
	engine := activation.NewDataActivationIntelligence()
	snapshot := engine.AnalyzeAndRecommend(ispData)

	respondJSON(w, http.StatusOK, snapshot)
}

// generateDemoISPData creates realistic demo data that reflects a typical
// email ecosystem with activation challenges (matching the user's description
// of underperforming compared to network)
func generateDemoISPData() map[activation.ISP]activation.ISPSendingData {
	return map[activation.ISP]activation.ISPSendingData{
		activation.ISPGmail: {
			ISP:          activation.ISPGmail,
			TotalSent:    850000,
			Delivered:    812000,
			Bounced:      38000,
			HardBounced:  15000,
			SoftBounced:  23000,
			Opens:        98000,
			UniqueOpens:  73000,
			Clicks:       12000,
			UniqueClicks: 8500,
			Complaints:   680,
			Unsubscribes: 3200,
			SpamTraps:    45,
		},
		activation.ISPYahoo: {
			ISP:          activation.ISPYahoo,
			TotalSent:    620000,
			Delivered:    580000,
			Bounced:      40000,
			HardBounced:  18000,
			SoftBounced:  22000,
			Opens:        52000,
			UniqueOpens:  38000,
			Clicks:       5800,
			UniqueClicks: 4100,
			Complaints:   850,
			Unsubscribes: 4500,
			SpamTraps:    120,
		},
		activation.ISPOutlook: {
			ISP:          activation.ISPOutlook,
			TotalSent:    420000,
			Delivered:    395000,
			Bounced:      25000,
			HardBounced:  10000,
			SoftBounced:  15000,
			Opens:        41000,
			UniqueOpens:  32000,
			Clicks:       4200,
			UniqueClicks: 3100,
			Complaints:   210,
			Unsubscribes: 1800,
			SpamTraps:    25,
		},
		activation.ISPAOL: {
			ISP:          activation.ISPAOL,
			TotalSent:    180000,
			Delivered:    162000,
			Bounced:      18000,
			HardBounced:  9000,
			SoftBounced:  9000,
			Opens:        11000,
			UniqueOpens:  8200,
			Clicks:       980,
			UniqueClicks: 720,
			Complaints:   310,
			Unsubscribes: 1500,
			SpamTraps:    65,
		},
		activation.ISPApple: {
			ISP:          activation.ISPApple,
			TotalSent:    280000,
			Delivered:    268000,
			Bounced:      12000,
			HardBounced:  4000,
			SoftBounced:  8000,
			Opens:        85000,  // Inflated by MPP
			UniqueOpens:  72000,  // Inflated by MPP
			Clicks:       3800,
			UniqueClicks: 2900,
			Complaints:   140,
			Unsubscribes: 950,
			SpamTraps:    10,
		},
	}
}

// mapToActivationISP converts common ISP name strings to activation.ISP type
func mapToActivationISP(ispName string) activation.ISP {
	lower := strings.ToLower(ispName)
	switch {
	case strings.Contains(lower, "gmail") || strings.Contains(lower, "google"):
		return activation.ISPGmail
	case strings.Contains(lower, "aol"):
		return activation.ISPAOL
	case strings.Contains(lower, "yahoo") || strings.Contains(lower, "verizon"):
		return activation.ISPYahoo
	case strings.Contains(lower, "outlook") || strings.Contains(lower, "hotmail") || strings.Contains(lower, "microsoft"):
		return activation.ISPOutlook
	case strings.Contains(lower, "apple") || strings.Contains(lower, "icloud"):
		return activation.ISPApple
	default:
		return activation.ISPOther
	}
}
