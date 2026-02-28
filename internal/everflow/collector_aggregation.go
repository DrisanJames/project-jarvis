package everflow

import (
	"context"
	"sort"
	"time"
)

// aggregateMetrics aggregates all metrics from clicks and conversions
func (c *Collector) aggregateMetrics(clicks []Click, conversions []Conversion) *CollectorMetrics {
	metrics := &CollectorMetrics{}
	todayStr := time.Now().Format("2006-01-02")

	// Aggregate by date
	dailyMap := make(map[string]*DailyPerformance)
	// Aggregate by offer
	offerMap := make(map[string]*OfferPerformance)
	// Aggregate by property
	propertyMap := make(map[string]*PropertyPerformance)
	// Aggregate by campaign (mailing ID)
	campaignMap := make(map[string]*CampaignRevenue)

	// Process clicks
	for _, click := range clicks {
		dateStr := click.Timestamp.Format("2006-01-02")

		// Daily aggregation
		if _, ok := dailyMap[dateStr]; !ok {
			dailyMap[dateStr] = &DailyPerformance{Date: dateStr}
		}
		dailyMap[dateStr].Clicks++

		// Today's metrics
		if dateStr == todayStr {
			metrics.TodayClicks++
		}

		// Offer aggregation
		offerKey := click.OfferID
		if _, ok := offerMap[offerKey]; !ok {
			offerMap[offerKey] = &OfferPerformance{
				OfferID:   click.OfferID,
				OfferName: click.OfferName,
			}
		}
		offerMap[offerKey].Clicks++

		// Property aggregation
		if click.PropertyCode != "" {
			if _, ok := propertyMap[click.PropertyCode]; !ok {
				propertyMap[click.PropertyCode] = &PropertyPerformance{
					PropertyCode: click.PropertyCode,
					PropertyName: click.PropertyName,
				}
			}
			propertyMap[click.PropertyCode].Clicks++
		}

		// Campaign aggregation
		if click.MailingID != "" {
			if _, ok := campaignMap[click.MailingID]; !ok {
				campaignMap[click.MailingID] = &CampaignRevenue{
					MailingID:    click.MailingID,
					CampaignName: click.Sub1, // Use raw sub1 as campaign name
					PropertyCode: click.PropertyCode,
					PropertyName: click.PropertyName,
					OfferID:      click.OfferID,
					OfferName:    click.OfferName,
				}
			}
			campaignMap[click.MailingID].Clicks++
		}
	}

	// Process conversions
	for _, conv := range conversions {
		dateStr := conv.ConversionTime.Format("2006-01-02")

		// Daily aggregation
		if _, ok := dailyMap[dateStr]; !ok {
			dailyMap[dateStr] = &DailyPerformance{Date: dateStr}
		}
		dailyMap[dateStr].Conversions++
		dailyMap[dateStr].Revenue += conv.Revenue
		dailyMap[dateStr].Payout += conv.Payout

		// Today's metrics
		if dateStr == todayStr {
			metrics.TodayConversions++
			metrics.TodayRevenue += conv.Revenue
			metrics.TodayPayout += conv.Payout
		}

		// Offer aggregation
		offerKey := conv.OfferID
		if _, ok := offerMap[offerKey]; !ok {
			offerMap[offerKey] = &OfferPerformance{
				OfferID:   conv.OfferID,
				OfferName: conv.OfferName,
			}
		}
		offerMap[offerKey].Conversions++
		offerMap[offerKey].Revenue += conv.Revenue
		offerMap[offerKey].Payout += conv.Payout

		// Property aggregation
		if conv.PropertyCode != "" {
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

		// Campaign aggregation
		if conv.MailingID != "" {
			if _, ok := campaignMap[conv.MailingID]; !ok {
				campaignMap[conv.MailingID] = &CampaignRevenue{
					MailingID:    conv.MailingID,
					CampaignName: conv.Sub1, // Use raw sub1 as campaign name
					PropertyCode: conv.PropertyCode,
					PropertyName: conv.PropertyName,
					OfferID:      conv.OfferID,
					OfferName:    conv.OfferName,
				}
			}
			// Update CampaignName if not set yet (in case conversion came before click)
			if campaignMap[conv.MailingID].CampaignName == "" && conv.Sub1 != "" {
				campaignMap[conv.MailingID].CampaignName = conv.Sub1
			}
			campaignMap[conv.MailingID].Conversions++
			campaignMap[conv.MailingID].Revenue += conv.Revenue
			campaignMap[conv.MailingID].Payout += conv.Payout
		}
	}

	// Calculate rates for daily metrics
	dailyList := make([]DailyPerformance, 0, len(dailyMap))
	for _, d := range dailyMap {
		if d.Clicks > 0 {
			d.ConversionRate = float64(d.Conversions) / float64(d.Clicks)
			d.EPC = d.Revenue / float64(d.Clicks)
		}
		dailyList = append(dailyList, *d)
	}
	// Sort by date descending
	sort.Slice(dailyList, func(i, j int) bool {
		return dailyList[i].Date > dailyList[j].Date
	})
	metrics.DailyPerformance = dailyList

	// Calculate rates for offer metrics
	offerList := make([]OfferPerformance, 0, len(offerMap))
	for _, o := range offerMap {
		if o.Clicks > 0 {
			o.ConversionRate = float64(o.Conversions) / float64(o.Clicks)
			o.EPC = o.Revenue / float64(o.Clicks)
		}
		offerList = append(offerList, *o)
	}
	// Sort by revenue descending
	sort.Slice(offerList, func(i, j int) bool {
		return offerList[i].Revenue > offerList[j].Revenue
	})
	metrics.OfferPerformance = offerList

	// Calculate rates for property metrics and count unique offers
	propertyList := make([]PropertyPerformance, 0, len(propertyMap))
	for _, p := range propertyMap {
		if p.Clicks > 0 {
			p.ConversionRate = float64(p.Conversions) / float64(p.Clicks)
			p.EPC = p.Revenue / float64(p.Clicks)
		}
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
	// Sort by revenue descending
	sort.Slice(propertyList, func(i, j int) bool {
		return propertyList[i].Revenue > propertyList[j].Revenue
	})
	metrics.PropertyPerformance = propertyList

	// Calculate rates for campaign metrics
	campaignList := make([]CampaignRevenue, 0, len(campaignMap))
	for _, cr := range campaignMap {
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

	// Sort by revenue descending
	sort.Slice(campaignList, func(i, j int) bool {
		return campaignList[i].Revenue > campaignList[j].Revenue
	})
	metrics.CampaignRevenue = campaignList

	// Store recent clicks and conversions (last 100 each)
	if len(clicks) > 100 {
		metrics.RecentClicks = clicks[len(clicks)-100:]
	} else {
		metrics.RecentClicks = clicks
	}
	if len(conversions) > 100 {
		metrics.RecentConversions = conversions[len(conversions)-100:]
	} else {
		metrics.RecentConversions = conversions
	}

	return metrics
}
