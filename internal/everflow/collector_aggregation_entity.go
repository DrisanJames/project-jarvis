package everflow

import (
	"context"
	"fmt"
	"log"
	"sort"
	"time"
)

// aggregateMetricsWithEntityReports aggregates metrics using entity reports for daily, offer, and campaign stats
func (c *Collector) aggregateMetricsWithEntityReports(entityReportByDate *EntityReportResponse, entityReportByOffer *EntityReportResponse, entityReportBySub1 *EntityReportResponse, conversions []Conversion) *CollectorMetrics {
	metrics := &CollectorMetrics{}
	todayStr := time.Now().Format("2006-01-02")

	// Build offer click map from entity report by offer
	offerClickMap := make(map[string]*OfferPerformance)
	if entityReportByOffer != nil {
		for _, row := range entityReportByOffer.Table {
			var offerID, offerName string
			for _, col := range row.Columns {
				if col.ColumnType == "offer" {
					offerID = col.ID
					offerName = col.Label
					break
				}
			}
			if offerID == "" {
				continue
			}
			offerClickMap[offerID] = &OfferPerformance{
				OfferID:     offerID,
				OfferName:   offerName,
				Clicks:      row.Reporting.TotalClick,
				Conversions: row.Reporting.Conversions,
				Revenue:     row.Reporting.Revenue,
				Payout:      row.Reporting.Payout,
			}
			// Calculate rates
			if row.Reporting.TotalClick > 0 {
				offerClickMap[offerID].ConversionRate = float64(row.Reporting.Conversions) / float64(row.Reporting.TotalClick)
				offerClickMap[offerID].EPC = row.Reporting.Revenue / float64(row.Reporting.TotalClick)
			}
		}
	}

	// Build campaign and property maps from entity report by sub1 (more accurate than parsing individual conversions)
	// This gives us complete revenue data directly from Everflow
	campaignClickMap := make(map[string]int64)
	type sub1Data struct {
		Clicks      int64
		Conversions int64
		Revenue     float64
		Payout      float64
		Sub1        string
		Parsed      *ParsedSub1
	}
	sub1DataMap := make(map[string]*sub1Data) // key: mailing_id
	// Track different categories of unattributed revenue
	type unattribCategory struct {
		Revenue     float64
		Payout      float64
		Conversions int64
		Clicks      int64
	}
	unattribEmpty := &unattribCategory{}       // Empty sub1
	unattribParseError := &unattribCategory{}  // Couldn't parse sub1
	unattribNoMailing := &unattribCategory{}   // Parsed but no mailing ID
	unattribUnknownProp := &unattribCategory{} // Has mailing ID but unknown property
	// Track campaigns with mailing ID but unknown property for Ongage lookup
	type unknownPropCampaign struct {
		MailingID string
		Sub1      string
		Data      *sub1Data
	}
	var unknownPropCampaigns []unknownPropCampaign
	if entityReportBySub1 != nil {
		for _, row := range entityReportBySub1.Table {
			var sub1Value string
			for _, col := range row.Columns {
				if col.ColumnType == "sub1" {
					sub1Value = col.Label // sub1 value is in the label
					break
				}
			}
			if sub1Value == "" {
				// Track unattributed revenue (conversions without sub1)
				unattribEmpty.Revenue += row.Reporting.Revenue
				unattribEmpty.Payout += row.Reporting.Payout
				unattribEmpty.Conversions += row.Reporting.Conversions
				unattribEmpty.Clicks += row.Reporting.TotalClick
				continue
			}

			// Parse sub1 to extract mailing ID and property code
			parsed, err := ParseSub1(sub1Value)
			if err != nil {
				// Track unattributed revenue (unparseable sub1)
				unattribParseError.Revenue += row.Reporting.Revenue
				unattribParseError.Payout += row.Reporting.Payout
				unattribParseError.Conversions += row.Reporting.Conversions
				unattribParseError.Clicks += row.Reporting.TotalClick
				continue
			}

			if parsed.MailingID == "" {
				// Track unattributed revenue (no mailing ID)
				unattribNoMailing.Revenue += row.Reporting.Revenue
				unattribNoMailing.Payout += row.Reporting.Payout
				unattribNoMailing.Conversions += row.Reporting.Conversions
				unattribNoMailing.Clicks += row.Reporting.TotalClick
				continue
			}

			// Check if property code is known
			isKnownProperty := ValidatePropertyCode(parsed.PropertyCode)

			campaignClickMap[parsed.MailingID] = row.Reporting.TotalClick
			data := &sub1Data{
				Clicks:      row.Reporting.TotalClick,
				Conversions: row.Reporting.Conversions,
				Revenue:     row.Reporting.Revenue,
				Payout:      row.Reporting.Payout,
				Sub1:        sub1Value,
				Parsed:      parsed,
			}
			sub1DataMap[parsed.MailingID] = data

			// If property is unknown but we have mailing ID, queue for Ongage lookup
			if !isKnownProperty && parsed.MailingID != "" {
				unknownPropCampaigns = append(unknownPropCampaigns, unknownPropCampaign{
					MailingID: parsed.MailingID,
					Sub1:      sub1Value,
					Data:      data,
				})
				// Also track in unknown property category (will be resolved if Ongage lookup succeeds)
				unattribUnknownProp.Revenue += row.Reporting.Revenue
				unattribUnknownProp.Payout += row.Reporting.Payout
				unattribUnknownProp.Conversions += row.Reporting.Conversions
				unattribUnknownProp.Clicks += row.Reporting.TotalClick
			}
		}
	}

	offerMap := make(map[string]*OfferPerformance)
	propertyMap := make(map[string]*PropertyPerformance)
	campaignMap := make(map[string]*CampaignRevenue)

	// Build daily performance from entity report (accurate click counts)
	dailyMap := make(map[string]*DailyPerformance)
	if entityReportByDate != nil {
		for _, row := range entityReportByDate.Table {
			// Extract date from columns
			var dateUnix int64
			for _, col := range row.Columns {
				if col.ColumnType == "date" {
					fmt.Sscanf(col.ID, "%d", &dateUnix)
					break
				}
			}
			if dateUnix == 0 {
				continue
			}

			dateTime := time.Unix(dateUnix, 0)
			dateStr := dateTime.Format("2006-01-02")

			dailyMap[dateStr] = &DailyPerformance{
				Date:        dateStr,
				Clicks:      row.Reporting.TotalClick,
				Conversions: row.Reporting.Conversions,
				Revenue:     row.Reporting.Revenue,
				Payout:      row.Reporting.Payout,
			}

			// Calculate rates
			if row.Reporting.TotalClick > 0 {
				dailyMap[dateStr].ConversionRate = float64(row.Reporting.Conversions) / float64(row.Reporting.TotalClick)
				dailyMap[dateStr].EPC = row.Reporting.Revenue / float64(row.Reporting.TotalClick)
			}

			// Today's metrics
			if dateStr == todayStr {
				metrics.TodayClicks = row.Reporting.TotalClick
				metrics.TodayConversions = row.Reporting.Conversions
				metrics.TodayRevenue = row.Reporting.Revenue
				metrics.TodayPayout = row.Reporting.Payout
			}
		}
	}

	// Process conversions for offer name enrichment only
	convOfferNames := make(map[string]string) // offerID -> offerName from conversions
	for _, conv := range conversions {
		if conv.OfferName != "" {
			convOfferNames[conv.OfferID] = conv.OfferName
		}
	}

	// Resolve unknown properties via Ongage campaign name lookup
	resolvedFromOngage := make(map[string]string) // mailingID -> resolved property code
	if c.campaignEnricher != nil && len(unknownPropCampaigns) > 0 {
		mailingIDs := make([]string, 0, len(unknownPropCampaigns))
		for _, campaign := range unknownPropCampaigns {
			mailingIDs = append(mailingIDs, campaign.MailingID)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		resolvedFromOngage = c.campaignEnricher.ResolvePropertyCodes(ctx, mailingIDs)
		cancel()
	}

	// Build property and campaign aggregations from entity report by sub1 (accurate totals)
	for mailingID, data := range sub1DataMap {
		propertyCode := data.Parsed.PropertyCode
		propertyName := data.Parsed.PropertyName
		isKnownProperty := ValidatePropertyCode(propertyCode)

		// Campaign aggregation from entity report
		campaignMap[mailingID] = &CampaignRevenue{
			MailingID:    mailingID,
			CampaignName: data.Sub1,
			PropertyCode: propertyCode,
			PropertyName: propertyName,
			OfferID:      data.Parsed.OfferID,
			Clicks:       data.Clicks,
			Conversions:  data.Conversions,
			Revenue:      data.Revenue,
			Payout:       data.Payout,
		}

		// Try to get offer name from conversions
		if name, ok := convOfferNames[data.Parsed.OfferID]; ok {
			campaignMap[mailingID].OfferName = name
		}

		// Property aggregation - only for known properties
		if isKnownProperty && propertyCode != "" {
			if _, ok := propertyMap[propertyCode]; !ok {
				propertyMap[propertyCode] = &PropertyPerformance{
					PropertyCode: propertyCode,
					PropertyName: propertyName,
				}
			}
			propertyMap[propertyCode].Clicks += data.Clicks
			propertyMap[propertyCode].Conversions += data.Conversions
			propertyMap[propertyCode].Revenue += data.Revenue
			propertyMap[propertyCode].Payout += data.Payout
		}

		// Track resolved property codes from Ongage
		if resolved, ok := resolvedFromOngage[mailingID]; ok && resolved != "" {
			// Update campaign with resolved property
			campaignMap[mailingID].PropertyCode = resolved
			campaignMap[mailingID].PropertyName = GetPropertyName(resolved)

			// Add to property aggregation
			if _, ok := propertyMap[resolved]; !ok {
				propertyMap[resolved] = &PropertyPerformance{
					PropertyCode: resolved,
					PropertyName: GetPropertyName(resolved),
				}
			}
			propertyMap[resolved].Clicks += data.Clicks
			propertyMap[resolved].Conversions += data.Conversions
			propertyMap[resolved].Revenue += data.Revenue
			propertyMap[resolved].Payout += data.Payout

			// Remove from unknown property tracking since it's now resolved
			unattribUnknownProp.Revenue -= data.Revenue
			unattribUnknownProp.Payout -= data.Payout
			unattribUnknownProp.Conversions -= data.Conversions
			unattribUnknownProp.Clicks -= data.Clicks
		}
	}

	// Fallback: If sub1 entity report wasn't available, use individual conversions for property/campaign breakdown
	if entityReportBySub1 == nil || len(sub1DataMap) == 0 {
		log.Println("Everflow: Using fallback conversion-based property/campaign aggregation (sub1 entity report not available)")
		campaignsNeedingResolution := make(map[string]bool) // mailingID -> needs resolution
		for _, conv := range conversions {
			if conv.MailingID != "" {
				if _, ok := campaignMap[conv.MailingID]; !ok {
					campaignMap[conv.MailingID] = &CampaignRevenue{
						MailingID:    conv.MailingID,
						CampaignName: conv.Sub1,
						PropertyCode: conv.PropertyCode,
						PropertyName: conv.PropertyName,
						OfferID:      conv.OfferID,
						OfferName:    conv.OfferName,
					}
				}
				campaignMap[conv.MailingID].Conversions++
				campaignMap[conv.MailingID].Revenue += conv.Revenue
				campaignMap[conv.MailingID].Payout += conv.Payout

				// Mark for resolution if property is unknown/empty
				if !ValidatePropertyCode(conv.PropertyCode) {
					campaignsNeedingResolution[conv.MailingID] = true
				}
			}
		}

		// Resolve property codes via Ongage for campaigns with mailing IDs but unknown properties
		if c.campaignEnricher != nil && len(campaignsNeedingResolution) > 0 {
			mailingIDs := make([]string, 0, len(campaignsNeedingResolution))
			for id := range campaignsNeedingResolution {
				mailingIDs = append(mailingIDs, id)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			resolved := c.campaignEnricher.ResolvePropertyCodes(ctx, mailingIDs)
			cancel()

			// Update campaigns and aggregate to properties with resolved codes
			for mailingID, propertyCode := range resolved {
				if campaign, ok := campaignMap[mailingID]; ok {
					campaign.PropertyCode = propertyCode
					campaign.PropertyName = GetPropertyName(propertyCode)

					// Add to property map
					if _, ok := propertyMap[propertyCode]; !ok {
						propertyMap[propertyCode] = &PropertyPerformance{
							PropertyCode: propertyCode,
							PropertyName: GetPropertyName(propertyCode),
						}
					}
					propertyMap[propertyCode].Conversions += campaign.Conversions
					propertyMap[propertyCode].Revenue += campaign.Revenue
					propertyMap[propertyCode].Payout += campaign.Payout

					// Remove from "needs resolution" so we don't count as unattributed
					delete(campaignsNeedingResolution, mailingID)
				}
			}
		}

		// Second pass: Aggregate remaining conversions to properties
		for _, conv := range conversions {
			// Skip if this campaign was resolved via Ongage
			if conv.MailingID != "" {
				if _, needsResolution := campaignsNeedingResolution[conv.MailingID]; !needsResolution {
					// Already handled via Ongage resolution, or has known property
					if ValidatePropertyCode(conv.PropertyCode) {
						// Known property - aggregate
						if _, ok := propertyMap[conv.PropertyCode]; !ok {
							propertyMap[conv.PropertyCode] = &PropertyPerformance{
								PropertyCode: conv.PropertyCode,
								PropertyName: conv.PropertyName,
							}
						}
						propertyMap[conv.PropertyCode].Conversions++
						propertyMap[conv.PropertyCode].Revenue += conv.Revenue
						propertyMap[conv.PropertyCode].Payout += conv.Payout
					}
					continue
				}
				// Campaign has mailing ID but couldn't be resolved - track as unattributed with mailing
				unattribUnknownProp.Conversions++
				unattribUnknownProp.Revenue += conv.Revenue
				unattribUnknownProp.Payout += conv.Payout
			} else if conv.PropertyCode == "" {
				// No mailing ID and no property code
				unattribEmpty.Conversions++
				unattribEmpty.Revenue += conv.Revenue
				unattribEmpty.Payout += conv.Payout
			} else if !ValidatePropertyCode(conv.PropertyCode) {
				// Has property code but it's unknown
				unattribUnknownProp.Conversions++
				unattribUnknownProp.Revenue += conv.Revenue
				unattribUnknownProp.Payout += conv.Payout
			}
		}
	}

	// Add categorized unattributed revenue entries
	// Combine empty sub1 and parse errors into one "Unattributed" category
	totalUnattribRevenue := unattribEmpty.Revenue + unattribParseError.Revenue + unattribNoMailing.Revenue
	totalUnattribPayout := unattribEmpty.Payout + unattribParseError.Payout + unattribNoMailing.Payout
	totalUnattribConversions := unattribEmpty.Conversions + unattribParseError.Conversions + unattribNoMailing.Conversions
	totalUnattribClicks := unattribEmpty.Clicks + unattribParseError.Clicks + unattribNoMailing.Clicks

	if totalUnattribRevenue > 0 {
		propertyMap["UNATTRIBUTED"] = &PropertyPerformance{
			PropertyCode:   "UNATTRIBUTED",
			PropertyName:   "Unattributed",
			Clicks:         totalUnattribClicks,
			Conversions:    totalUnattribConversions,
			Revenue:        totalUnattribRevenue,
			Payout:         totalUnattribPayout,
			IsUnattributed: true,
			UnattribReason: UnattribReasonEmptySub1,
		}
	}

	// Add unknown property category (has mailing ID but unknown property abbreviation)
	if unattribUnknownProp.Revenue > 0 {
		propertyMap["UNKNOWN_PROPERTY"] = &PropertyPerformance{
			PropertyCode:   "UNKNOWN_PROPERTY",
			PropertyName:   "Unknown Property",
			Clicks:         unattribUnknownProp.Clicks,
			Conversions:    unattribUnknownProp.Conversions,
			Revenue:        unattribUnknownProp.Revenue,
			Payout:         unattribUnknownProp.Payout,
			IsUnattributed: true,
			UnattribReason: UnattribReasonUnknownProperty,
		}
	}

	// Add any offers from entity report that weren't in conversions (offers with clicks but no conversions)
	for offerID, offerData := range offerClickMap {
		if _, ok := offerMap[offerID]; !ok {
			offerMap[offerID] = offerData
		}
		// Enrich with name from conversions if available
		if name, ok := convOfferNames[offerID]; ok && offerMap[offerID].OfferName == "" {
			offerMap[offerID].OfferName = name
		}
	}

	// Build daily list from map
	dailyList := make([]DailyPerformance, 0, len(dailyMap))
	for _, d := range dailyMap {
		dailyList = append(dailyList, *d)
	}
	// Sort by date descending
	sort.Slice(dailyList, func(i, j int) bool {
		return dailyList[i].Date > dailyList[j].Date
	})
	metrics.DailyPerformance = dailyList

	// Build offer list with click data from entity report
	offerList := make([]OfferPerformance, 0, len(offerMap))
	for _, o := range offerMap {
		offerList = append(offerList, *o)
	}
	sort.Slice(offerList, func(i, j int) bool {
		return offerList[i].Revenue > offerList[j].Revenue
	})
	metrics.OfferPerformance = offerList

	// Build property list and count unique offers
	propertyList := make([]PropertyPerformance, 0, len(propertyMap))
	for _, p := range propertyMap {
		// Count unique offers for this property
		uniqueOffers := make(map[string]bool)
		for _, conv := range conversions {
			if conv.PropertyCode == p.PropertyCode {
				uniqueOffers[conv.OfferID] = true
			}
		}
		p.UniqueOffers = len(uniqueOffers)
		propertyList = append(propertyList, *p)
	}
	sort.Slice(propertyList, func(i, j int) bool {
		return propertyList[i].Revenue > propertyList[j].Revenue
	})
	metrics.PropertyPerformance = propertyList

	// Build campaign list with rate calculations (clicks already set from entity report)
	campaignList := make([]CampaignRevenue, 0, len(campaignMap))
	for _, cr := range campaignMap {
		// Calculate conversion rate and EPC if there are clicks
		if cr.Clicks > 0 {
			cr.ConversionRate = float64(cr.Conversions) / float64(cr.Clicks)
			cr.EPC = cr.Revenue / float64(cr.Clicks)
		}
		campaignList = append(campaignList, *cr)
	}

	// Enrich campaigns with Ongage data (audience size, sending domain, etc.)
	if c.campaignEnricher != nil {
		ctx := context.Background()
		campaignList = c.campaignEnricher.EnrichCampaigns(ctx, campaignList)
	}

	sort.Slice(campaignList, func(i, j int) bool {
		return campaignList[i].Revenue > campaignList[j].Revenue
	})
	metrics.CampaignRevenue = campaignList

	// Calculate revenue by ESP (SparkPost, Mailgun, SES, etc.)
	// Now includes offer-based attribution to capture CPM and unattributed revenue
	metrics.ESPRevenue = c.calculateESPRevenue(campaignList, offerList)

	// Calculate CPM vs Non-CPM revenue breakdown
	metrics.RevenueBreakdown = c.calculateRevenueBreakdown(offerList, conversions)

	// Store recent conversions (last 100)
	if len(conversions) > 100 {
		metrics.RecentConversions = conversions[len(conversions)-100:]
	} else {
		metrics.RecentConversions = conversions
	}

	return metrics
}
