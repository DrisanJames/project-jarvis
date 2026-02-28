package everflow

import (
	"log"
	"sort"
)

// calculateRevenueBreakdown calculates CPM vs Non-CPM revenue breakdown
func (c *Collector) calculateRevenueBreakdown(offers []OfferPerformance, conversions []Conversion) *RevenueBreakdown {
	breakdown := &RevenueBreakdown{
		DailyTrend: []DailyBreakdown{},
	}

	// Build offer type lookup map
	offerTypeMap := make(map[string]bool) // true = CPM, false = Non-CPM
	for _, o := range offers {
		offerTypeMap[o.OfferID] = IsCPMOffer(o.OfferName)
	}

	// Calculate totals from offers
	for _, o := range offers {
		if IsCPMOffer(o.OfferName) {
			breakdown.CPM.OfferCount++
			breakdown.CPM.Clicks += o.Clicks
			breakdown.CPM.Conversions += o.Conversions
			breakdown.CPM.Revenue += o.Revenue
			breakdown.CPM.Payout += o.Payout
		} else {
			breakdown.NonCPM.OfferCount++
			breakdown.NonCPM.Clicks += o.Clicks
			breakdown.NonCPM.Conversions += o.Conversions
			breakdown.NonCPM.Revenue += o.Revenue
			breakdown.NonCPM.Payout += o.Payout
		}
	}

	// Calculate percentages
	totalRevenue := breakdown.CPM.Revenue + breakdown.NonCPM.Revenue
	if totalRevenue > 0 {
		breakdown.CPM.Percentage = (breakdown.CPM.Revenue / totalRevenue) * 100
		breakdown.NonCPM.Percentage = (breakdown.NonCPM.Revenue / totalRevenue) * 100
	}

	// Calculate daily trend from conversions
	dailyMap := make(map[string]*DailyBreakdown)
	for _, conv := range conversions {
		dateStr := conv.ConversionTime.Format("2006-01-02")
		if _, ok := dailyMap[dateStr]; !ok {
			dailyMap[dateStr] = &DailyBreakdown{Date: dateStr}
		}

		isCPM := IsCPMOffer(conv.OfferName)
		if isCPM {
			dailyMap[dateStr].CPMRevenue += conv.Revenue
		} else {
			dailyMap[dateStr].NonCPMRevenue += conv.Revenue
		}
	}

	// Convert map to sorted list
	for _, d := range dailyMap {
		breakdown.DailyTrend = append(breakdown.DailyTrend, *d)
	}
	sort.Slice(breakdown.DailyTrend, func(i, j int) bool {
		return breakdown.DailyTrend[i].Date > breakdown.DailyTrend[j].Date
	})

	return breakdown
}

// normalizeESPNameForDisplay normalizes ESP names to a canonical display form
// This groups variants together (e.g., "SparkPost Enterprise" -> "SparkPost")
func normalizeESPNameForDisplay(name string) string {
	switch name {
	case "SparkPost", "SparkPost Enterprise", "SparkPost Momentum":
		return "SparkPost"
	case "Amazon SES":
		return "Amazon SES"
	case "Mailgun":
		return "Mailgun"
	default:
		return name
	}
}

// calculateESPRevenue calculates revenue breakdown by ESP (SparkPost, Mailgun, SES, etc.)
// It combines conversion-based attribution with offer-based attribution to capture all revenue
func (c *Collector) calculateESPRevenue(campaigns []CampaignRevenue, offers []OfferPerformance) []ESPRevenuePerformance {
	// STEP 1: Conversion-based attribution (what we had before)
	// Group campaigns by ESP based on Ongage-linked campaigns
	espMap := make(map[string]*ESPRevenuePerformance)
	var conversionBasedRevenue float64

	for _, campaign := range campaigns {
		if !campaign.OngageLinked {
			continue // Skip campaigns without Ongage data
		}

		espName := campaign.ESPName
		if espName == "" {
			espName = "Unknown"
		}

		// Normalize ESP name to group variants together (e.g., SparkPost Enterprise -> SparkPost)
		normalizedName := normalizeESPNameForDisplay(espName)

		if _, ok := espMap[normalizedName]; !ok {
			espMap[normalizedName] = &ESPRevenuePerformance{
				ESPName: normalizedName,
			}
		}

		esp := espMap[normalizedName]
		esp.CampaignCount++
		esp.TotalSent += campaign.Sent
		esp.TotalDelivered += campaign.Delivered
		esp.TotalOpens += campaign.UniqueOpens
		esp.Clicks += campaign.Clicks
		esp.Conversions += campaign.Conversions
		esp.Revenue += campaign.Revenue
		esp.Payout += campaign.Payout

		conversionBasedRevenue += campaign.Revenue
	}

	// STEP 2: Offer-based attribution to capture CPM and unattributed revenue
	// Calculate total offer revenue (this is the TRUE total from Everflow)
	var totalOfferRevenue float64
	for _, offer := range offers {
		totalOfferRevenue += offer.Revenue
	}

	// Check if there's unattributed revenue
	unattributedRevenue := totalOfferRevenue - conversionBasedRevenue
	attributedViaOffers := false

	if unattributedRevenue > 0.01 && c.campaignEnricher != nil {
		log.Printf("ESP Revenue: Found $%.2f unattributed revenue (total: $%.2f, conversion-based: $%.2f)",
			unattributedRevenue, totalOfferRevenue, conversionBasedRevenue)

		// Get Ongage campaigns to build offer-to-ESP volume mapping
		ongageCampaigns := c.campaignEnricher.GetOngageCampaigns()
		if len(ongageCampaigns) > 0 {
			// Build offer-to-ESP volume mapping
			offerESPVolumes := BuildOfferESPVolumeMap(ongageCampaigns)
			log.Printf("ESP Revenue: Built offer-ESP volume map for %d offers from %d campaigns",
				len(offerESPVolumes), len(ongageCampaigns))

			// Attribute offer revenue to ESPs based on send volume
			offerAttributions := AttributeOfferRevenueToESPs(offers, offerESPVolumes)

			// Aggregate to ESP level
			offerBasedESPRevenue := AggregateOfferAttributionToESPs(offerAttributions)

			// Calculate scaling factor: we only want to add the UNATTRIBUTED portion
			// offerBasedESPRevenue represents TOTAL offer revenue by ESP
			// We scale it down to just the unattributed portion
			var totalOfferBasedRevenue float64
			for _, esp := range offerBasedESPRevenue {
				totalOfferBasedRevenue += esp.Revenue
			}

			if totalOfferBasedRevenue > 0 {
				scaleFactor := unattributedRevenue / totalOfferBasedRevenue
				log.Printf("ESP Revenue: Scaling factor %.4f to distribute $%.2f unattributed revenue",
					scaleFactor, unattributedRevenue)

				// Add scaled unattributed revenue to each ESP
				for espName, offerESP := range offerBasedESPRevenue {
					additionalRevenue := offerESP.Revenue * scaleFactor
					additionalPayout := offerESP.Payout * scaleFactor

					if existingESP, ok := espMap[espName]; ok {
						existingESP.Revenue += additionalRevenue
						existingESP.Payout += additionalPayout
						log.Printf("ESP Revenue: Added $%.2f unattributed to %s (total now: $%.2f)",
							additionalRevenue, espName, existingESP.Revenue)
					} else {
						// Create new ESP entry for ESPs that had no conversion-based revenue
						espMap[espName] = &ESPRevenuePerformance{
							ESPName:   espName,
							Revenue:   additionalRevenue,
							Payout:    additionalPayout,
							TotalSent: offerESP.TotalSent,
						}
						log.Printf("ESP Revenue: Created new ESP %s with $%.2f unattributed revenue",
							espName, additionalRevenue)
					}
				}
				attributedViaOffers = true
			}
		} else {
			log.Printf("ESP Revenue: No Ongage campaigns available for offer-based attribution")
		}
	}

	// STEP 2b: If we still have unattributed revenue after offer-based attribution,
	// distribute it proportionally to existing ESPs, or create an "Unattributed" entry
	var currentESPTotal float64
	for _, esp := range espMap {
		currentESPTotal += esp.Revenue
	}

	remainingUnattributed := totalOfferRevenue - currentESPTotal
	if remainingUnattributed > 0.01 { // More than 1 cent
		if len(espMap) > 0 && !attributedViaOffers {
			// Distribute proportionally to existing ESPs
			scaleFactor := totalOfferRevenue / currentESPTotal
			log.Printf("ESP Revenue: Scaling existing ESPs by %.4f to account for $%.2f unattributed (total should be $%.2f)",
				scaleFactor, remainingUnattributed, totalOfferRevenue)

			for _, esp := range espMap {
				esp.Revenue *= scaleFactor
				esp.Payout *= scaleFactor
			}
		} else if len(espMap) == 0 {
			// No ESP data at all - put everything in "Unattributed"
			log.Printf("ESP Revenue: No ESP attribution possible, creating Unattributed entry for $%.2f", totalOfferRevenue)
			espMap["Unattributed"] = &ESPRevenuePerformance{
				ESPName: "Unattributed",
				Revenue: totalOfferRevenue,
			}
			// Also calculate payout proportionally from offers
			var totalOfferPayout float64
			for _, offer := range offers {
				totalOfferPayout += offer.Payout
			}
			espMap["Unattributed"].Payout = totalOfferPayout
		}
	}

	// STEP 3: Calculate rates, percentages, and cost metrics
	// Recalculate total revenue after merging
	var totalRevenue float64
	for _, esp := range espMap {
		totalRevenue += esp.Revenue
	}

	result := make([]ESPRevenuePerformance, 0, len(espMap))
	for _, esp := range espMap {
		// Calculate percentage of total revenue
		if totalRevenue > 0 {
			esp.Percentage = (esp.Revenue / totalRevenue) * 100
		}

		// Calculate average eCPM (revenue per 1000 emails)
		if esp.TotalDelivered > 0 {
			esp.AvgECPM = (esp.Revenue / float64(esp.TotalDelivered)) * 1000
		}

		// Calculate conversion rate
		if esp.Clicks > 0 {
			esp.ConversionRate = float64(esp.Conversions) / float64(esp.Clicks)
			esp.EPC = esp.Revenue / float64(esp.Clicks)
		}

		// Calculate cost metrics if cost calculator is available
		if c.costCalculator != nil {
			hasContract := c.costCalculator.HasContract(esp.ESPName)
			if hasContract {
				esp.CostMetrics = c.costCalculator.CalculateCosts(
					esp.ESPName,
					c.lookbackDays,
					esp.TotalSent,
					esp.Revenue,
				)
			}
		}

		result = append(result, *esp)
	}

	// Sort by revenue descending
	sort.Slice(result, func(i, j int) bool {
		return result[i].Revenue > result[j].Revenue
	})

	log.Printf("ESP Revenue: Final attribution - %d ESPs, $%.2f total (was $%.2f conversion-based)",
		len(result), totalRevenue, conversionBasedRevenue)

	return result
}

// espCostPlaygroundDefaults computes the default volume and cost eCPM values
// used by the cost playground UI. Extracted to keep buildDataPartnerAnalytics lean.
func (c *Collector) espCostPlaygroundDefaults(totalESPSends int64) (defaultVolume int64, defaultCostECPM, totalESPCost float64) {
	defaultVolume = totalESPSends
	if c.metrics != nil && c.metrics.ESPRevenue != nil {
		for _, esp := range c.metrics.ESPRevenue {
			if esp.CostMetrics != nil {
				totalESPCost += esp.CostMetrics.TotalCost
			}
		}
	}
	if defaultVolume > 0 {
		defaultCostECPM = totalESPCost / float64(defaultVolume) * 1000
	}
	return
}

