package agent

import (
	"strings"
	"time"
)

func (o *OpenAIAgent) getOngageCampaigns(topN int, espFilter string) []map[string]interface{} {
	if o.agent.collectors == nil || o.agent.collectors.Ongage == nil {
		return nil
	}
	
	campaigns := o.agent.collectors.Ongage.GetCampaigns()
	var results []map[string]interface{}
	
	for _, c := range campaigns {
		if espFilter != "" && !strings.EqualFold(c.ESP, espFilter) {
			continue
		}
		results = append(results, map[string]interface{}{
			"id":            c.ID,
			"name":          c.Name,
			"subject":       c.Subject,
			"esp":           c.ESP,
			"targeted":      c.Targeted,
			"sent":          c.Sent,
			"delivered":     c.Delivered,
			"opens":         c.Opens,
			"unique_opens":  c.UniqueOpens,
			"clicks":        c.Clicks,
			"unique_clicks": c.UniqueClicks,
			"delivery_rate": c.DeliveryRate,
			"open_rate":     c.OpenRate,
			"click_rate":    c.ClickRate,
			"schedule_time": c.ScheduleTime,
		})
		if len(results) >= topN {
			break
		}
	}
	
	return results
}

func (o *OpenAIAgent) getSubjectLineAnalysis(topN int, perfFilter string) []map[string]interface{} {
	if o.agent.collectors == nil || o.agent.collectors.Ongage == nil {
		return nil
	}
	
	subjects := o.agent.collectors.Ongage.GetSubjectAnalysis()
	var results []map[string]interface{}
	
	for _, s := range subjects {
		if perfFilter != "" && !strings.EqualFold(s.Performance, perfFilter) {
			continue
		}
		results = append(results, map[string]interface{}{
			"subject":        s.Subject,
			"campaign_count": s.CampaignCount,
			"total_sent":     s.TotalSent,
			"avg_open_rate":  s.AvgOpenRate,
			"avg_click_rate": s.AvgClickRate,
			"performance":    s.Performance,
			"has_emoji":      s.HasEmoji,
			"has_number":     s.HasNumber,
			"has_urgency":    s.HasUrgency,
			"has_question":   s.HasQuestion,
			"length":         s.Length,
		})
		if len(results) >= topN {
			break
		}
	}
	
	return results
}

func (o *OpenAIAgent) getSendTimeAnalysis() map[string]interface{} {
	if o.agent.collectors == nil || o.agent.collectors.Ongage == nil {
		return nil
	}
	
	schedule := o.agent.collectors.Ongage.GetScheduleAnalysis()
	
	var optimalTimes []map[string]interface{}
	dayPerformance := make(map[string][]float64)
	
	for _, s := range schedule {
		dayPerformance[s.DayName] = append(dayPerformance[s.DayName], s.AvgOpenRate)
		
		if s.Performance == "optimal" || s.Performance == "good" {
			optimalTimes = append(optimalTimes, map[string]interface{}{
				"day":            s.DayName,
				"hour":           s.Hour,
				"avg_open_rate":  s.AvgOpenRate,
				"avg_click_rate": s.AvgClickRate,
				"campaign_count": s.CampaignCount,
				"performance":    s.Performance,
			})
		}
	}
	
	dayAvg := make(map[string]float64)
	for day, rates := range dayPerformance {
		var sum float64
		for _, r := range rates {
			sum += r
		}
		dayAvg[day] = sum / float64(len(rates))
	}
	
	return map[string]interface{}{
		"optimal_times":      optimalTimes,
		"day_averages":       dayAvg,
		"total_time_slots":   len(schedule),
	}
}

func (o *OpenAIAgent) getAudienceSegments(minCampaigns int) []map[string]interface{} {
	if o.agent.collectors == nil || o.agent.collectors.Ongage == nil {
		return nil
	}
	
	segments := o.agent.collectors.Ongage.GetAudienceAnalysis()
	var results []map[string]interface{}
	
	for _, s := range segments {
		if s.CampaignCount < minCampaigns {
			continue
		}
		results = append(results, map[string]interface{}{
			"segment_id":     s.SegmentID,
			"segment_name":   s.SegmentName,
			"campaign_count": s.CampaignCount,
			"total_targeted": s.TotalTargeted,
			"total_sent":     s.TotalSent,
			"avg_open_rate":  s.AvgOpenRate,
			"avg_click_rate": s.AvgClickRate,
			"engagement":     s.Engagement,
		})
	}
	
	return results
}

func (o *OpenAIAgent) getPipelineMetrics(days int) []map[string]interface{} {
	if o.agent.collectors == nil || o.agent.collectors.Ongage == nil {
		return nil
	}
	
	pipeline := o.agent.collectors.Ongage.GetPipelineMetrics()
	var results []map[string]interface{}
	
	cutoff := time.Now().AddDate(0, 0, -days).Format("2006-01-02")
	for _, p := range pipeline {
		if p.Date >= cutoff {
			results = append(results, map[string]interface{}{
				"date":           p.Date,
				"total_targeted": p.TotalTargeted,
				"total_sent":     p.TotalSent,
				"total_delivered": p.TotalDelivered,
				"delivery_rate":  p.DeliveryRate,
				"total_opens":    p.TotalOpens,
				"open_rate":      p.OpenRate,
				"total_clicks":   p.TotalClicks,
				"click_rate":     p.ClickRate,
			})
		}
	}
	
	return results
}
