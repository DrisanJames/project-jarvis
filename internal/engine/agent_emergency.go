package engine

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// EmergencyAgent detects spike conditions within this ISP's traffic.
// Any threshold breach triggers immediate pause of all queues and
// requires manual resume.
type EmergencyAgent struct {
	BaseAgent
	memory      *MemoryStore
	convictions *ConvictionStore
	alertCh     chan<- Decision
}

// NewEmergencyAgent creates a new ISP-scoped emergency agent.
func NewEmergencyAgent(id AgentID, config ISPConfig, memory *MemoryStore, convictions *ConvictionStore, alertCh chan<- Decision) *EmergencyAgent {
	return &EmergencyAgent{
		BaseAgent: BaseAgent{
			ID:     id,
			Config: config,
			Status: StatusActive,
		},
		memory:      memory,
		convictions: convictions,
		alertCh:     alertCh,
	}
}

// Evaluate checks for emergency spike conditions.
func (a *EmergencyAgent) Evaluate(snap SignalSnapshot) []Decision {
	if a.GetStatus() != StatusActive {
		return nil
	}
	a.MarkEvaluated()

	var decisions []Decision
	now := time.Now()
	tc := TemporalContext(now)
	ctx := context.Background()

	emergency := false
	trigger := ""

	if snap.BounceRate5m > 25.0 {
		emergency = true
		trigger = "bounce_spike"
	}

	if snap.DeferralRate5m > 50.0 {
		emergency = true
		trigger = "deferral_spike"
	}

	if snap.ComplaintRate1h > 1.0 {
		emergency = true
		trigger = "complaint_spike"
	}

	criticalIPs := 0
	var criticalIPList []string
	for ip, m := range snap.IPMetrics {
		if m.Score < 50 {
			criticalIPs++
			criticalIPList = append(criticalIPList, fmt.Sprintf("%s(score=%.0f)", ip, m.Score))
		}
	}
	if criticalIPs >= 3 {
		emergency = true
		trigger = "coordinated_reputation_attack"
	}

	if !emergency {
		return nil
	}

	a.SetStatus(StatusFiring)

	var affectedIPs []string
	for ip := range snap.IPMetrics {
		affectedIPs = append(affectedIPs, ip)
	}

	incident := IncidentReport{
		ISP:     a.ID.ISP,
		Trigger: trigger,
		TriggerMetrics: mustJSON(map[string]interface{}{
			"bounce_rate_5m":    snap.BounceRate5m,
			"deferral_rate_5m":  snap.DeferralRate5m,
			"complaint_rate_1h": snap.ComplaintRate1h,
			"critical_ips":      criticalIPs,
		}),
		AffectedIPs:     affectedIPs,
		AffectedDomains: []string{string(a.ID.ISP)},
		StartedAt:       now,
		DetectedAt:      now,
		ActionsTaken:    []string{"pause_all_queues", "disable_all_sources", "send_incident_report"},
		Status:          "active",
	}

	if a.memory != nil {
		a.memory.AppendIncident(ctx, a.ID.ISP, incident)
		a.memory.FlushImmediate(ctx)
	}

	decisions = append(decisions, Decision{
		ISP:         a.ID.ISP,
		AgentType:   AgentEmergency,
		ActionTaken: "emergency_halt",
		ActionParams: mustJSON(map[string]interface{}{
			"trigger":      trigger,
			"bounce_5m":    snap.BounceRate5m,
			"deferral_5m":  snap.DeferralRate5m,
			"complaint_1h": snap.ComplaintRate1h,
			"affected_ips": affectedIPs,
		}),
		TargetType:  "isp",
		TargetValue: string(a.ID.ISP),
		Result:      "pending",
		CreatedAt:   now,
	})

	// Record a WONT conviction with full incident context
	if a.convictions != nil {
		ipSummary := strings.Join(affectedIPs, ", ")
		if len(affectedIPs) > 5 {
			ipSummary = fmt.Sprintf("%s... (%d total)", strings.Join(affectedIPs[:5], ", "), len(affectedIPs))
		}

		a.convictions.Record(ctx, Conviction{
			AgentType: AgentEmergency,
			ISP:       a.ID.ISP,
			Verdict:   VerdictWont,
			Statement: fmt.Sprintf(
				"I WONT allow any traffic to %s. EMERGENCY: %s. "+
				"Bounce %.1f%% (5m), deferral %.1f%% (5m), complaint %.2f%% (1h). "+
				"%d sent in 5m, %d bounced in 1h, %d deferred in 5m. "+
				"Critical IPs: %s. Affected IPs: %s. "+
				"All queues halted. Manual resume required. DSN: %s.",
				a.ID.ISP, trigger,
				snap.BounceRate5m, snap.DeferralRate5m, snap.ComplaintRate1h,
				snap.Sent5m, snap.Bounced1h, snap.Deferred5m,
				strings.Join(criticalIPList, ", "), ipSummary,
				strings.Join(snap.RecentDSNCodes, ", "),
			),
			Context: MicroContext{
				Date: tc.Date, DayOfWeek: tc.DayOfWeek, HourUTC: tc.HourUTC,
				IsHoliday: tc.IsHoliday, HolidayName: tc.HolidayName,
				Pool:            a.Config.PoolName,
				AttemptedVolume: snap.Sent5m,
				BounceRate:      snap.BounceRate5m,
				DeferralRate:    snap.DeferralRate5m,
				ComplaintRate:   snap.ComplaintRate1h,
				BounceCount:     snap.Bounced1h,
				DeferralCount:   snap.Deferred5m,
				ComplaintCount:  snap.Complaints1h,
				AcceptedCount:   snap.Accepted1h,
				DSNCodes:        snap.RecentDSNCodes,
				DSNDiagnostics:  snap.RecentDSNDiagnostics,
				Reason:          trigger,
			},
			CreatedAt: now,
		})
	}

	for _, d := range decisions {
		if a.alertCh != nil {
			select {
			case a.alertCh <- d:
			default:
			}
		}
	}

	return decisions
}
