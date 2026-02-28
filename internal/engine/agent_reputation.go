package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// ReputationAgent monitors bounce rates and complaint rates per IP,
// scoped to a single ISP. Escalates from warning to action (disable source,
// quarantine IP) based on ISP-specific thresholds.
type ReputationAgent struct {
	BaseAgent
	memory      *MemoryStore
	convictions *ConvictionStore
	alertCh     chan<- Decision
}

// NewReputationAgent creates a new ISP-scoped reputation agent.
func NewReputationAgent(id AgentID, config ISPConfig, memory *MemoryStore, convictions *ConvictionStore, alertCh chan<- Decision) *ReputationAgent {
	return &ReputationAgent{
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

// Evaluate checks the current signal snapshot against ISP thresholds.
func (a *ReputationAgent) Evaluate(snap SignalSnapshot) []Decision {
	if a.GetStatus() != StatusActive || a.IsOnCooldown() {
		return nil
	}
	a.MarkEvaluated()

	var decisions []Decision
	now := time.Now()
	tc := TemporalContext(now)
	ctx := context.Background()

	for ip, metrics := range snap.IPMetrics {
		if metrics.BounceRate1h > a.Config.BounceActionPct {
			decisions = append(decisions, Decision{
				ISP:         a.ID.ISP,
				AgentType:   AgentReputation,
				ActionTaken: "disable_source_ip",
				ActionParams: mustJSON(map[string]interface{}{
					"bounce_rate": metrics.BounceRate1h,
					"threshold":   a.Config.BounceActionPct,
				}),
				TargetType:  "ip",
				TargetValue: ip,
				Result:      "pending",
				CreatedAt:   now,
			})

			if a.convictions != nil {
				a.convictions.Record(ctx, Conviction{
					AgentType: AgentReputation,
					ISP:       a.ID.ISP,
					Verdict:   VerdictWont,
					Statement: fmt.Sprintf(
						"I WONT allow IP %s to send to %s. Bounce rate %.2f%% exceeded action threshold %.2f%%. %d sent, %d bounced over 1h. Disabled.",
						ip, a.ID.ISP, metrics.BounceRate1h, a.Config.BounceActionPct, metrics.Sent1h, metrics.Bounced1h,
					),
					Context: MicroContext{
						Date: tc.Date, DayOfWeek: tc.DayOfWeek, HourUTC: tc.HourUTC,
						IsHoliday: tc.IsHoliday, HolidayName: tc.HolidayName,
						IP: ip, Pool: a.Config.PoolName,
						AttemptedVolume: metrics.Sent1h,
						BounceRate:      metrics.BounceRate1h,
						ComplaintRate:   metrics.ComplaintRate,
						DeferralRate:    metrics.DeferralRate,
						BounceCount:     metrics.Bounced1h,
						AcceptedCount:   metrics.Accepted1h,
						IPScore:         metrics.Score,
						DSNCodes:        snap.RecentDSNCodes,
					},
					CreatedAt: now,
				})
			}

		} else if metrics.BounceRate1h > a.Config.BounceWarnPct {
			decisions = append(decisions, Decision{
				ISP:         a.ID.ISP,
				AgentType:   AgentReputation,
				ActionTaken: "warn_bounce_rate",
				ActionParams: mustJSON(map[string]interface{}{
					"bounce_rate": metrics.BounceRate1h,
					"threshold":   a.Config.BounceWarnPct,
				}),
				TargetType:  "ip",
				TargetValue: ip,
				Result:      "applied",
				CreatedAt:   now,
			})
		}

		if metrics.ComplaintRate > a.Config.ComplaintActionPct {
			decisions = append(decisions, Decision{
				ISP:         a.ID.ISP,
				AgentType:   AgentReputation,
				ActionTaken: "quarantine_ip",
				ActionParams: mustJSON(map[string]interface{}{
					"complaint_rate": metrics.ComplaintRate,
					"threshold":      a.Config.ComplaintActionPct,
				}),
				TargetType:  "ip",
				TargetValue: ip,
				Result:      "pending",
				CreatedAt:   now,
			})

			if a.convictions != nil {
				a.convictions.Record(ctx, Conviction{
					AgentType: AgentReputation,
					ISP:       a.ID.ISP,
					Verdict:   VerdictWont,
					Statement: fmt.Sprintf(
						"I WONT keep IP %s active on %s-pool. Complaint rate %.3f%% exceeded threshold %.3f%%. %d complaints over 24h against %d sent in 1h. Quarantined.",
						ip, a.ID.ISP, metrics.ComplaintRate, a.Config.ComplaintActionPct, metrics.Complaints24h, metrics.Sent1h,
					),
					Context: MicroContext{
						Date: tc.Date, DayOfWeek: tc.DayOfWeek, HourUTC: tc.HourUTC,
						IsHoliday: tc.IsHoliday, HolidayName: tc.HolidayName,
						IP: ip, Pool: a.Config.PoolName,
						AttemptedVolume: metrics.Sent1h,
						BounceRate:      metrics.BounceRate1h,
						ComplaintRate:   metrics.ComplaintRate,
						DeferralRate:    metrics.DeferralRate,
						ComplaintCount:  metrics.Complaints24h,
						AcceptedCount:   metrics.Accepted1h,
						IPScore:         metrics.Score,
					},
					CreatedAt: now,
				})
			}

		} else if metrics.ComplaintRate > a.Config.ComplaintWarnPct {
			decisions = append(decisions, Decision{
				ISP:         a.ID.ISP,
				AgentType:   AgentReputation,
				ActionTaken: "warn_complaint_rate",
				ActionParams: mustJSON(map[string]interface{}{
					"complaint_rate": metrics.ComplaintRate,
					"threshold":      a.Config.ComplaintWarnPct,
				}),
				TargetType:  "ip",
				TargetValue: ip,
				Result:      "applied",
				CreatedAt:   now,
			})
		}

		// WILL conviction: IP is performing well, record the positive observation
		if metrics.Score >= 85 && metrics.Sent1h >= 100 &&
			metrics.BounceRate1h <= a.Config.BounceWarnPct &&
			metrics.ComplaintRate <= a.Config.ComplaintWarnPct {
			if a.convictions != nil {
				acceptRate := 0.0
				if metrics.Sent1h > 0 {
					acceptRate = float64(metrics.Accepted1h) / float64(metrics.Sent1h) * 100
				}
				a.convictions.Record(ctx, Conviction{
					AgentType: AgentReputation,
					ISP:       a.ID.ISP,
					Verdict:   VerdictWill,
					Statement: fmt.Sprintf(
						"I WILL keep IP %s active on %s-pool. Score %.1f, bounce %.2f%%, complaint %.3f%%, acceptance %.1f%%. %d messages over 1h. Performing well.",
						ip, a.ID.ISP, metrics.Score, metrics.BounceRate1h, metrics.ComplaintRate, acceptRate, metrics.Sent1h,
					),
					Context: MicroContext{
						Date: tc.Date, DayOfWeek: tc.DayOfWeek, HourUTC: tc.HourUTC,
						IsHoliday: tc.IsHoliday, HolidayName: tc.HolidayName,
						IP: ip, Pool: a.Config.PoolName,
						AttemptedVolume: metrics.Sent1h,
						BounceRate:      metrics.BounceRate1h,
						ComplaintRate:   metrics.ComplaintRate,
						DeferralRate:    metrics.DeferralRate,
						AcceptanceRate:  acceptRate,
						AcceptedCount:   metrics.Accepted1h,
						BounceCount:     metrics.Bounced1h,
						IPScore:         metrics.Score,
					},
					CreatedAt: now,
				})
			}
		}
	}

	// Global ISP-level bounce rate check
	if snap.BounceRate1h > a.Config.BounceActionPct {
		decisions = append(decisions, Decision{
			ISP:         a.ID.ISP,
			AgentType:   AgentReputation,
			ActionTaken: "pause_isp_queues",
			ActionParams: mustJSON(map[string]interface{}{
				"bounce_rate": snap.BounceRate1h,
				"threshold":   a.Config.BounceActionPct,
			}),
			TargetType:  "isp",
			TargetValue: string(a.ID.ISP),
			Result:      "pending",
			CreatedAt:   now,
		})

		if a.convictions != nil {
			a.convictions.Record(ctx, Conviction{
				AgentType: AgentReputation,
				ISP:       a.ID.ISP,
				Verdict:   VerdictWont,
				Statement: fmt.Sprintf(
					"I WONT allow any traffic to %s. Global bounce rate %.2f%% exceeded %.2f%%. %d sent, %d bounced, %d accepted over 1h. All queues paused.",
					a.ID.ISP, snap.BounceRate1h, a.Config.BounceActionPct, snap.Sent1h, snap.Bounced1h, snap.Accepted1h,
				),
				Context: MicroContext{
					Date: tc.Date, DayOfWeek: tc.DayOfWeek, HourUTC: tc.HourUTC,
					IsHoliday: tc.IsHoliday, HolidayName: tc.HolidayName,
					Pool:            a.Config.PoolName,
					AttemptedVolume: snap.Sent1h,
					BounceRate:      snap.BounceRate1h,
					ComplaintRate:   snap.ComplaintRate1h,
					DeferralRate:    snap.DeferralRate1h,
					BounceCount:     snap.Bounced1h,
					AcceptedCount:   snap.Accepted1h,
					DSNCodes:        snap.RecentDSNCodes,
					DSNDiagnostics:  snap.RecentDSNDiagnostics,
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

func mustJSON(v interface{}) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(fmt.Sprintf(`{"error":"%s"}`, err))
	}
	return data
}
