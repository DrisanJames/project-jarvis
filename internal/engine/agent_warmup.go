package engine

import (
	"context"
	"fmt"
	"time"
)

// WarmupAgent manages IP warmup schedules for this ISP. Monitors daily
// volume, bounce, and complaint rates against graduation criteria.
type WarmupAgent struct {
	BaseAgent
	memory      *MemoryStore
	convictions *ConvictionStore
	alertCh     chan<- Decision
}

// NewWarmupAgent creates a new ISP-scoped warmup agent.
func NewWarmupAgent(id AgentID, config ISPConfig, memory *MemoryStore, convictions *ConvictionStore, alertCh chan<- Decision) *WarmupAgent {
	return &WarmupAgent{
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

// Evaluate checks warmup progress and decides on advancement/pausing.
func (a *WarmupAgent) Evaluate(snap SignalSnapshot) []Decision {
	if a.GetStatus() != StatusActive || a.IsOnCooldown() {
		return nil
	}
	a.MarkEvaluated()

	var decisions []Decision
	now := time.Now()
	tc := TemporalContext(now)
	ctx := context.Background()

	if snap.BounceRate1h > 5.0 {
		decisions = append(decisions, Decision{
			ISP:         a.ID.ISP,
			AgentType:   AgentWarmup,
			ActionTaken: "pause_warmup",
			ActionParams: mustJSON(map[string]interface{}{
				"bounce_rate": snap.BounceRate1h,
				"reason":      "bounce_rate_exceeded",
				"hold_hours":  48,
			}),
			TargetType:  "pool",
			TargetValue: "warmup-pool",
			Result:      "pending",
			CreatedAt:   now,
		})

		if a.convictions != nil {
			a.convictions.Record(ctx, Conviction{
				AgentType: AgentWarmup,
				ISP:       a.ID.ISP,
				Verdict:   VerdictWont,
				Statement: fmt.Sprintf(
					"I WONT advance warmup for %s. Bounce rate %.2f%% exceeded 5%% threshold. "+
					"%d sent, %d bounced, %d accepted over 1h. Holding for 48h.",
					a.ID.ISP, snap.BounceRate1h,
					snap.Sent1h, snap.Bounced1h, snap.Accepted1h,
				),
				Context: MicroContext{
					Date: tc.Date, DayOfWeek: tc.DayOfWeek, HourUTC: tc.HourUTC,
					IsHoliday: tc.IsHoliday, HolidayName: tc.HolidayName,
					Pool:            "warmup-pool",
					AttemptedVolume: snap.Sent1h,
					BounceRate:      snap.BounceRate1h,
					ComplaintRate:   snap.ComplaintRate1h,
					BounceCount:     snap.Bounced1h,
					AcceptedCount:   snap.Accepted1h,
					DSNCodes:        snap.RecentDSNCodes,
				},
				CreatedAt: now,
			})
		}
	}

	if snap.ComplaintRate1h > 0.06 {
		decisions = append(decisions, Decision{
			ISP:         a.ID.ISP,
			AgentType:   AgentWarmup,
			ActionTaken: "pause_warmup",
			ActionParams: mustJSON(map[string]interface{}{
				"complaint_rate": snap.ComplaintRate1h,
				"reason":         "complaint_rate_exceeded",
				"hold_hours":     72,
			}),
			TargetType:  "pool",
			TargetValue: "warmup-pool",
			Result:      "pending",
			CreatedAt:   now,
		})

		if a.convictions != nil {
			a.convictions.Record(ctx, Conviction{
				AgentType: AgentWarmup,
				ISP:       a.ID.ISP,
				Verdict:   VerdictWont,
				Statement: fmt.Sprintf(
					"I WONT advance warmup for %s. Complaint rate %.3f%% exceeded 0.06%% threshold. "+
					"%d sent, %d complaints over 1h. Holding for 72h — complaints are poison during warmup.",
					a.ID.ISP, snap.ComplaintRate1h,
					snap.Sent1h, snap.Complaints1h,
				),
				Context: MicroContext{
					Date: tc.Date, DayOfWeek: tc.DayOfWeek, HourUTC: tc.HourUTC,
					IsHoliday: tc.IsHoliday, HolidayName: tc.HolidayName,
					Pool:            "warmup-pool",
					AttemptedVolume: snap.Sent1h,
					BounceRate:      snap.BounceRate1h,
					ComplaintRate:   snap.ComplaintRate1h,
					ComplaintCount:  snap.Complaints1h,
					AcceptedCount:   snap.Accepted1h,
				},
				CreatedAt: now,
			})
		}
	}

	// Graduation check: bounce < 2% and complaint < 0.03%
	if snap.BounceRate1h < 2.0 && snap.ComplaintRate1h < 0.03 {
		decisions = append(decisions, Decision{
			ISP:         a.ID.ISP,
			AgentType:   AgentWarmup,
			ActionTaken: "advance_warmup_day",
			ActionParams: mustJSON(map[string]interface{}{
				"bounce_rate":    snap.BounceRate1h,
				"complaint_rate": snap.ComplaintRate1h,
			}),
			TargetType:  "pool",
			TargetValue: "warmup-pool",
			Result:      "pending",
			CreatedAt:   now,
		})

		if a.convictions != nil {
			acceptRate := 0.0
			if snap.Sent1h > 0 {
				acceptRate = float64(snap.Accepted1h) / float64(snap.Sent1h) * 100
			}
			a.convictions.Record(ctx, Conviction{
				AgentType: AgentWarmup,
				ISP:       a.ID.ISP,
				Verdict:   VerdictWill,
				Statement: fmt.Sprintf(
					"I WILL advance warmup for %s. Bounce %.2f%% (< 2%%), complaint %.3f%% (< 0.03%%). "+
					"%d sent, %d accepted (%.1f%%) over 1h. Graduating to next warmup tier.",
					a.ID.ISP, snap.BounceRate1h, snap.ComplaintRate1h,
					snap.Sent1h, snap.Accepted1h, acceptRate,
				),
				Context: MicroContext{
					Date: tc.Date, DayOfWeek: tc.DayOfWeek, HourUTC: tc.HourUTC,
					IsHoliday: tc.IsHoliday, HolidayName: tc.HolidayName,
					Pool:            "warmup-pool",
					AttemptedVolume: snap.Sent1h,
					BounceRate:      snap.BounceRate1h,
					ComplaintRate:   snap.ComplaintRate1h,
					AcceptanceRate:  acceptRate,
					AcceptedCount:   snap.Accepted1h,
					BounceCount:     snap.Bounced1h,
				},
				CreatedAt: now,
			})
		}
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
