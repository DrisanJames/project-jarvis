package engine

import (
	"context"
	"fmt"
	"time"
)

// PoolAgent manages IP assignment to this ISP's pool. Computes per-IP
// performance scores and moves IPs between ISP pool, warmup, and quarantine.
type PoolAgent struct {
	BaseAgent
	memory      *MemoryStore
	convictions *ConvictionStore
	alertCh     chan<- Decision
}

// NewPoolAgent creates a new ISP-scoped pool manager agent.
func NewPoolAgent(id AgentID, config ISPConfig, memory *MemoryStore, convictions *ConvictionStore, alertCh chan<- Decision) *PoolAgent {
	return &PoolAgent{
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

// Evaluate checks per-IP scores and makes pool assignment decisions.
func (a *PoolAgent) Evaluate(snap SignalSnapshot) []Decision {
	if a.GetStatus() != StatusActive || a.IsOnCooldown() {
		return nil
	}
	a.MarkEvaluated()

	var decisions []Decision
	now := time.Now()
	tc := TemporalContext(now)
	ctx := context.Background()

	for ip, metrics := range snap.IPMetrics {
		score := metrics.Score

		if score < 50 {
			decisions = append(decisions, Decision{
				ISP:         a.ID.ISP,
				AgentType:   AgentPool,
				ActionTaken: "quarantine_ip",
				ActionParams: mustJSON(map[string]interface{}{
					"score":          score,
					"bounce_rate":    metrics.BounceRate1h,
					"complaint_rate": metrics.ComplaintRate,
					"deferral_rate":  metrics.DeferralRate,
					"from_pool":      a.Config.PoolName,
					"to_pool":        "quarantine-pool",
				}),
				TargetType:  "ip",
				TargetValue: ip,
				Result:      "pending",
				CreatedAt:   now,
			})

			if a.convictions != nil {
				a.convictions.Record(ctx, Conviction{
					AgentType: AgentPool,
					ISP:       a.ID.ISP,
					Verdict:   VerdictWont,
					Statement: fmt.Sprintf(
						"I WONT keep IP %s in %s. Score %.1f — below quarantine threshold 50. "+
						"Bounce %.2f%% (%d), complaint %.3f%% (%d), deferral %.1f%% (%d). "+
						"%d sent over 1h. Moved to quarantine-pool.",
						ip, a.Config.PoolName, score,
						metrics.BounceRate1h, metrics.Bounced1h,
						metrics.ComplaintRate, metrics.Complaints24h,
						metrics.DeferralRate, metrics.Deferred5m,
						metrics.Sent1h,
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
						DeferralCount:   metrics.Deferred5m,
						ComplaintCount:  metrics.Complaints24h,
						AcceptedCount:   metrics.Accepted1h,
						IPScore:         score,
						FromPool:        a.Config.PoolName,
						ToPool:          "quarantine-pool",
					},
					CreatedAt: now,
				})
			}

		} else if score < 70 {
			decisions = append(decisions, Decision{
				ISP:         a.ID.ISP,
				AgentType:   AgentPool,
				ActionTaken: "reduce_ip_volume",
				ActionParams: mustJSON(map[string]interface{}{
					"score":       score,
					"bounce_rate": metrics.BounceRate1h,
				}),
				TargetType:  "ip",
				TargetValue: ip,
				Result:      "pending",
				CreatedAt:   now,
			})

			if a.convictions != nil {
				a.convictions.Record(ctx, Conviction{
					AgentType: AgentPool,
					ISP:       a.ID.ISP,
					Verdict:   VerdictWont,
					Statement: fmt.Sprintf(
						"I WONT allow full volume for IP %s in %s. Score %.1f — below healthy threshold 70. "+
						"Bounce %.2f%%, deferral %.1f%%. Reducing volume allocation. %d sent over 1h.",
						ip, a.Config.PoolName, score,
						metrics.BounceRate1h, metrics.DeferralRate, metrics.Sent1h,
					),
					Context: MicroContext{
						Date: tc.Date, DayOfWeek: tc.DayOfWeek, HourUTC: tc.HourUTC,
						IsHoliday: tc.IsHoliday, HolidayName: tc.HolidayName,
						IP: ip, Pool: a.Config.PoolName,
						AttemptedVolume: metrics.Sent1h,
						BounceRate:      metrics.BounceRate1h,
						DeferralRate:    metrics.DeferralRate,
						AcceptedCount:   metrics.Accepted1h,
						IPScore:         score,
					},
					CreatedAt: now,
				})
			}

		} else if score >= 85 && metrics.Sent1h >= 100 {
			// Healthy IP — record a positive conviction
			if a.convictions != nil {
				acceptRate := 0.0
				if metrics.Sent1h > 0 {
					acceptRate = float64(metrics.Accepted1h) / float64(metrics.Sent1h) * 100
				}
				a.convictions.Record(ctx, Conviction{
					AgentType: AgentPool,
					ISP:       a.ID.ISP,
					Verdict:   VerdictWill,
					Statement: fmt.Sprintf(
						"I WILL keep IP %s in %s. Score %.1f — healthy. "+
						"Bounce %.2f%%, complaint %.3f%%, acceptance %.1f%%. %d messages over 1h.",
						ip, a.Config.PoolName, score,
						metrics.BounceRate1h, metrics.ComplaintRate, acceptRate, metrics.Sent1h,
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
						IPScore:         score,
					},
					CreatedAt: now,
				})
			}
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
