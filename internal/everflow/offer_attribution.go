package everflow

import (
	"log"
	"strings"

	"github.com/ignite/sparkpost-monitor/internal/ongage"
)

// OfferESPVolume tracks email volume by ESP for each offer
type OfferESPVolume struct {
	OfferID     string             `json:"offer_id"`
	OfferName   string             `json:"offer_name"`
	TotalSent   int64              `json:"total_sent"`
	ESPVolumes  map[string]int64   `json:"esp_volumes"`   // ESP name -> sent volume
	ESPPercents map[string]float64 `json:"esp_percents"`  // ESP name -> percentage of total
}

// OfferESPAttribution represents revenue attributed to an ESP from an offer
type OfferESPAttribution struct {
	ESPName    string  `json:"esp_name"`
	OfferID    string  `json:"offer_id"`
	OfferName  string  `json:"offer_name"`
	Revenue    float64 `json:"revenue"`
	Payout     float64 `json:"payout"`
	SentVolume int64   `json:"sent_volume"`
	Percentage float64 `json:"percentage"` // Percentage of offer sent via this ESP
}

// BuildOfferESPVolumeMap builds a map of offer_id -> ESP volumes from Ongage campaigns
// This allows us to attribute offer revenue to ESPs based on campaign send volume
func BuildOfferESPVolumeMap(campaigns []ongage.ProcessedCampaign) map[string]*OfferESPVolume {
	offerMap := make(map[string]*OfferESPVolume)

	for _, campaign := range campaigns {
		// Skip campaigns with no sends or no ESP
		if campaign.Sent == 0 || campaign.ESP == "" {
			continue
		}

		// Parse campaign name to extract offer ID
		_, _, offerID, offerName, _, err := ParseCampaignName(campaign.Name)
		if err != nil || offerID == "" {
			// Can't parse offer ID from this campaign
			continue
		}

		// Normalize ESP name for grouping
		espName := normalizeESPNameForDisplay(campaign.ESP)

		// Initialize offer entry if needed
		if _, ok := offerMap[offerID]; !ok {
			offerMap[offerID] = &OfferESPVolume{
				OfferID:     offerID,
				OfferName:   offerName,
				TotalSent:   0,
				ESPVolumes:  make(map[string]int64),
				ESPPercents: make(map[string]float64),
			}
		}

		offer := offerMap[offerID]
		offer.TotalSent += campaign.Sent
		offer.ESPVolumes[espName] += campaign.Sent
	}

	// Calculate percentages
	for _, offer := range offerMap {
		if offer.TotalSent > 0 {
			for esp, volume := range offer.ESPVolumes {
				offer.ESPPercents[esp] = float64(volume) / float64(offer.TotalSent) * 100
			}
		}
	}

	return offerMap
}

// AttributeOfferRevenueToESPs distributes offer revenue to ESPs based on send volume
// This captures CPM and other offer-level revenue that isn't tracked at campaign/conversion level
func AttributeOfferRevenueToESPs(offers []OfferPerformance, offerESPVolumes map[string]*OfferESPVolume) []OfferESPAttribution {
	var attributions []OfferESPAttribution

	for _, offer := range offers {
		// Try to find ESP volume data for this offer
		offerVolume, ok := offerESPVolumes[offer.OfferID]
		if !ok || offerVolume.TotalSent == 0 {
			// No campaign data for this offer - can't attribute
			continue
		}

		// Distribute revenue to ESPs based on send volume percentage
		for espName, volume := range offerVolume.ESPVolumes {
			percentage := float64(volume) / float64(offerVolume.TotalSent)
			
			attributions = append(attributions, OfferESPAttribution{
				ESPName:    espName,
				OfferID:    offer.OfferID,
				OfferName:  offer.OfferName,
				Revenue:    offer.Revenue * percentage,
				Payout:     offer.Payout * percentage,
				SentVolume: volume,
				Percentage: percentage * 100,
			})
		}
	}

	return attributions
}

// AggregateOfferAttributionToESPs aggregates offer-level attributions by ESP
func AggregateOfferAttributionToESPs(attributions []OfferESPAttribution) map[string]*ESPRevenuePerformance {
	espMap := make(map[string]*ESPRevenuePerformance)

	for _, attr := range attributions {
		if _, ok := espMap[attr.ESPName]; !ok {
			espMap[attr.ESPName] = &ESPRevenuePerformance{
				ESPName: attr.ESPName,
			}
		}
		
		esp := espMap[attr.ESPName]
		esp.Revenue += attr.Revenue
		esp.Payout += attr.Payout
		esp.TotalSent += attr.SentVolume
	}

	return espMap
}

// MergeESPRevenue merges offer-based ESP revenue into existing conversion-based ESP revenue
// The offer-based revenue represents TOTAL offer revenue
// The conversion-based revenue represents revenue already attributed via sub1 tracking
// We need to calculate the difference to avoid double-counting
func MergeESPRevenue(conversionBased []ESPRevenuePerformance, offerBased map[string]*ESPRevenuePerformance, totalOfferRevenue float64) []ESPRevenuePerformance {
	// Calculate total revenue already attributed via conversions
	var conversionAttributedRevenue float64
	espMap := make(map[string]*ESPRevenuePerformance)
	
	for i := range conversionBased {
		esp := &conversionBased[i]
		espMap[esp.ESPName] = esp
		conversionAttributedRevenue += esp.Revenue
	}

	// Calculate the gap (unattributed revenue)
	unattributedRevenue := totalOfferRevenue - conversionAttributedRevenue
	
	if unattributedRevenue <= 0 {
		// All revenue is already attributed via conversions
		log.Printf("Offer Attribution: No unattributed revenue (conversion-based: $%.2f, total: $%.2f)", 
			conversionAttributedRevenue, totalOfferRevenue)
		return conversionBased
	}

	log.Printf("Offer Attribution: Attributing $%.2f unattributed revenue (total: $%.2f, conversion-based: $%.2f)",
		unattributedRevenue, totalOfferRevenue, conversionAttributedRevenue)

	// Calculate total offer-based revenue for scaling
	var totalOfferBasedRevenue float64
	for _, esp := range offerBased {
		totalOfferBasedRevenue += esp.Revenue
	}

	if totalOfferBasedRevenue == 0 {
		log.Printf("Offer Attribution: No offer-based ESP mapping available")
		return conversionBased
	}

	// Scale and add unattributed revenue proportionally
	scaleFactor := unattributedRevenue / totalOfferBasedRevenue
	
	for espName, offerESP := range offerBased {
		additionalRevenue := offerESP.Revenue * scaleFactor
		additionalPayout := offerESP.Payout * scaleFactor
		
		if existingESP, ok := espMap[espName]; ok {
			// Add to existing ESP
			existingESP.Revenue += additionalRevenue
			existingESP.Payout += additionalPayout
			// Note: We don't add TotalSent here since it's already counted from Ongage enrichment
			log.Printf("Offer Attribution: Added $%.2f to %s (now $%.2f total)", 
				additionalRevenue, espName, existingESP.Revenue)
		} else {
			// Create new ESP entry
			newESP := &ESPRevenuePerformance{
				ESPName:   espName,
				Revenue:   additionalRevenue,
				Payout:    additionalPayout,
				TotalSent: offerESP.TotalSent,
			}
			espMap[espName] = newESP
			log.Printf("Offer Attribution: Created new ESP %s with $%.2f revenue", espName, additionalRevenue)
		}
	}

	// Convert map back to slice
	result := make([]ESPRevenuePerformance, 0, len(espMap))
	for _, esp := range espMap {
		result = append(result, *esp)
	}

	return result
}

// matchOfferToOfferID tries to match an Everflow offer to campaigns by offer ID
// The offer name format is usually: "Offer Name (ID)" or "Offer Name CPM (ID)"
func ExtractOfferIDFromName(offerName string) string {
	// Look for pattern: "(123)" at the end
	if idx := strings.LastIndex(offerName, "("); idx != -1 {
		end := strings.Index(offerName[idx:], ")")
		if end != -1 {
			potentialID := strings.TrimSpace(offerName[idx+1 : idx+end])
			if isNumeric(potentialID) {
				return potentialID
			}
		}
	}
	return ""
}
