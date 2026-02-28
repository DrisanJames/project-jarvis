package agent

import "time"

func (o *OpenAIAgent) getRevenueSummary(days int) map[string]interface{} {
	if o.agent.collectors == nil || o.agent.collectors.Everflow == nil {
		return map[string]interface{}{"error": "Everflow not configured"}
	}
	
	daily := o.agent.collectors.Everflow.GetDailyPerformance()
	totalRevenue := o.agent.collectors.Everflow.GetTotalRevenue()
	
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
	
	result := map[string]interface{}{
		"total_revenue":     totalRevenue,
		"today_revenue":     todayRevenue,
		"total_clicks":      totalClicks,
		"total_conversions": totalConversions,
		"days_tracked":      len(daily),
	}
	
	if totalClicks > 0 {
		result["overall_epc"] = totalRevenue / float64(totalClicks)
		result["overall_conversion_rate"] = float64(totalConversions) / float64(totalClicks)
	}
	
	return result
}

func (o *OpenAIAgent) getDailyRevenue(days int) []map[string]interface{} {
	if o.agent.collectors == nil || o.agent.collectors.Everflow == nil {
		return nil
	}
	
	daily := o.agent.collectors.Everflow.GetDailyPerformance()
	var results []map[string]interface{}
	
	cutoff := time.Now().AddDate(0, 0, -days).Format("2006-01-02")
	for _, d := range daily {
		if d.Date >= cutoff {
			results = append(results, map[string]interface{}{
				"date":            d.Date,
				"revenue":         d.Revenue,
				"clicks":          d.Clicks,
				"conversions":     d.Conversions,
				"conversion_rate": d.ConversionRate,
				"epc":             d.EPC,
			})
		}
	}
	return results
}

func (o *OpenAIAgent) getOfferPerformance(topN int, sortBy string) []map[string]interface{} {
	if o.agent.collectors == nil || o.agent.collectors.Everflow == nil {
		return nil
	}
	
	offers := o.agent.collectors.Everflow.GetOfferPerformance()
	var results []map[string]interface{}
	
	for _, off := range offers {
		results = append(results, map[string]interface{}{
			"offer_id":        off.OfferID,
			"offer_name":      off.OfferName,
			"revenue":         off.Revenue,
			"clicks":          off.Clicks,
			"conversions":     off.Conversions,
			"conversion_rate": off.ConversionRate,
			"epc":             off.EPC,
		})
	}
	
	if len(results) > topN {
		results = results[:topN]
	}
	
	return results
}

func (o *OpenAIAgent) getPropertyPerformance(topN int) []map[string]interface{} {
	if o.agent.collectors == nil || o.agent.collectors.Everflow == nil {
		return nil
	}
	
	props := o.agent.collectors.Everflow.GetPropertyPerformance()
	var results []map[string]interface{}
	
	for _, p := range props {
		results = append(results, map[string]interface{}{
			"property_code":   p.PropertyCode,
			"property_name":   p.PropertyName,
			"revenue":         p.Revenue,
			"clicks":          p.Clicks,
			"conversions":     p.Conversions,
			"conversion_rate": p.ConversionRate,
			"epc":             p.EPC,
			"unique_offers":   p.UniqueOffers,
		})
	}
	
	if len(results) > topN {
		results = results[:topN]
	}
	
	return results
}

func (o *OpenAIAgent) getCampaignRevenue(topN int, minRevenue float64) []map[string]interface{} {
	if o.agent.collectors == nil || o.agent.collectors.Everflow == nil {
		return nil
	}
	
	campaigns := o.agent.collectors.Everflow.GetCampaignRevenue()
	var results []map[string]interface{}
	
	for _, c := range campaigns {
		if minRevenue > 0 && c.Revenue < minRevenue {
			continue
		}
		results = append(results, map[string]interface{}{
			"mailing_id":      c.MailingID,
			"campaign_name":   c.CampaignName,
			"revenue":         c.Revenue,
			"clicks":          c.Clicks,
			"conversions":     c.Conversions,
			"conversion_rate": c.ConversionRate,
			"epc":             c.EPC,
			"audience_size":   c.AudienceSize,
			"ecpm":            c.ECPM,
			"ongage_linked":   c.OngageLinked,
		})
		if len(results) >= topN {
			break
		}
	}
	
	return results
}
