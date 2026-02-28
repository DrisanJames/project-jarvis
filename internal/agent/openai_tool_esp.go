package agent

import "strings"

func (o *OpenAIAgent) getEcosystemSummary() map[string]interface{} {
	eco, _ := o.agent.getEcosystemData()
	return map[string]interface{}{
		"total_volume":     eco.TotalVolume,
		"total_delivered":  eco.TotalDelivered,
		"total_opens":      eco.TotalOpens,
		"total_clicks":     eco.TotalClicks,
		"total_bounces":    eco.TotalBounces,
		"total_complaints": eco.TotalComplaints,
		"delivery_rate":    eco.DeliveryRate,
		"open_rate":        eco.OpenRate,
		"click_rate":       eco.ClickRate,
		"bounce_rate":      eco.BounceRate,
		"complaint_rate":   eco.ComplaintRate,
		"provider_count":   eco.ProviderCount,
		"isp_count":        eco.ISPCount,
		"healthy_isps":     eco.HealthyISPs,
		"warning_isps":     eco.WarningISPs,
		"critical_isps":    eco.CriticalISPs,
	}
}

func (o *OpenAIAgent) getISPPerformance(ispFilter, providerFilter string) []map[string]interface{} {
	_, allISPs := o.agent.getEcosystemData()
	
	var results []map[string]interface{}
	for _, isp := range allISPs {
		if ispFilter != "" && !strings.Contains(strings.ToLower(isp.ISP), strings.ToLower(ispFilter)) {
			continue
		}
		if providerFilter != "" && !strings.EqualFold(isp.Provider, providerFilter) {
			continue
		}
		results = append(results, map[string]interface{}{
			"provider":       isp.Provider,
			"isp":            isp.ISP,
			"volume":         isp.Volume,
			"delivered":      isp.Delivered,
			"delivery_rate":  isp.DeliveryRate,
			"open_rate":      isp.OpenRate,
			"click_rate":     isp.ClickRate,
			"bounce_rate":    isp.BounceRate,
			"complaint_rate": isp.ComplaintRate,
			"status":         isp.Status,
			"status_reason":  isp.StatusReason,
		})
	}
	return results
}

func (o *OpenAIAgent) getProviderComparison() []map[string]interface{} {
	_, allISPs := o.agent.getEcosystemData()
	
	providerStats := make(map[string]map[string]interface{})
	for _, isp := range allISPs {
		if _, ok := providerStats[isp.Provider]; !ok {
			providerStats[isp.Provider] = map[string]interface{}{
				"provider":      isp.Provider,
				"volume":        int64(0),
				"delivered":     int64(0),
				"isp_count":     0,
				"healthy_count": 0,
				"warning_count": 0,
				"critical_count": 0,
			}
		}
		stats := providerStats[isp.Provider]
		stats["volume"] = stats["volume"].(int64) + isp.Volume
		stats["delivered"] = stats["delivered"].(int64) + isp.Delivered
		stats["isp_count"] = stats["isp_count"].(int) + 1
		switch isp.Status {
		case "healthy":
			stats["healthy_count"] = stats["healthy_count"].(int) + 1
		case "warning":
			stats["warning_count"] = stats["warning_count"].(int) + 1
		case "critical":
			stats["critical_count"] = stats["critical_count"].(int) + 1
		}
	}
	
	var results []map[string]interface{}
	for _, stats := range providerStats {
		vol := stats["volume"].(int64)
		del := stats["delivered"].(int64)
		if vol > 0 {
			stats["delivery_rate"] = float64(del) / float64(vol)
		}
		results = append(results, stats)
	}
	return results
}

func (o *OpenAIAgent) getActiveAlerts() map[string]interface{} {
	alerts := o.agent.GetAlerts()
	
	var activeAlerts []map[string]interface{}
	for _, a := range alerts {
		if !a.Acknowledged {
			activeAlerts = append(activeAlerts, map[string]interface{}{
				"id":          a.ID,
				"severity":    a.Severity,
				"category":    a.Category,
				"title":       a.Title,
				"description": a.Description,
				"entity_name": a.EntityName,
				"deviation":   a.Deviation,
				"recommendation": a.Recommendation,
				"timestamp":   a.Timestamp,
			})
		}
	}
	
	_, allISPs := o.agent.getEcosystemData()
	var ispConcerns []map[string]interface{}
	for _, isp := range allISPs {
		if isp.Status == "critical" || isp.Status == "warning" {
			ispConcerns = append(ispConcerns, map[string]interface{}{
				"provider":      isp.Provider,
				"isp":           isp.ISP,
				"status":        isp.Status,
				"status_reason": isp.StatusReason,
				"complaint_rate": isp.ComplaintRate,
				"bounce_rate":   isp.BounceRate,
			})
		}
	}
	
	return map[string]interface{}{
		"alerts":       activeAlerts,
		"isp_concerns": ispConcerns,
		"alert_count":  len(activeAlerts),
		"concern_count": len(ispConcerns),
	}
}

func (o *OpenAIAgent) getLearnedPatterns() map[string]interface{} {
	baselines := o.agent.GetBaselines()
	correlations := o.agent.GetCorrelations()
	
	var correlationList []map[string]interface{}
	for _, c := range correlations {
		correlationList = append(correlationList, map[string]interface{}{
			"entity_name":       c.EntityName,
			"trigger_metric":    c.TriggerMetric,
			"trigger_threshold": c.TriggerThreshold,
			"effect_metric":     c.EffectMetric,
			"effect_change":     c.EffectChange,
			"confidence":        c.Confidence,
			"occurrences":       c.Occurrences,
		})
	}
	
	return map[string]interface{}{
		"baselines_learned":   len(baselines),
		"correlations_found":  len(correlations),
		"correlation_details": correlationList,
	}
}
