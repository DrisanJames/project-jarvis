package engine

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"
)

// wontQuarantineReemitInterval bounds how often a still-quarantined IP's
// "WONT keep IP" conviction is re-recorded. The decision itself is emitted
// every tick; only the conviction (ring + log line) is deduplicated.
const wontQuarantineReemitInterval = time.Hour

// convictionDedupDisabled restores per-tick WONT-quarantine convictions
// (pre-2026-11-04 behaviour) when ENGINE_CONVICTION_DEDUP_DISABLED=1.
func convictionDedupDisabled() bool {
	return os.Getenv("ENGINE_CONVICTION_DEDUP_DISABLED") == "1"
}

// wontQuarantineGate records a WONT-quarantine conviction for an IP only on
// the transition into quarantine, or at most once per
// wontQuarantineReemitInterval while the IP stays below threshold. Agents are
// ISP-scoped, so the key is the IP; each statement kind uses its own gate
// instance so one kind never masks another. The zero value is ready to use.
// An IP absent from snap.IPMetrics is not cleared, so if it returns below
// threshold within the interval its re-entry WONT is delayed (<=1h), not lost.
type wontQuarantineGate struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// allow reports whether the WONT conviction for ip should be recorded now.
func (g *wontQuarantineGate) allow(ip string, now time.Time) bool {
	if convictionDedupDisabled() {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.last == nil {
		g.last = make(map[string]time.Time)
	}
	if t, ok := g.last[ip]; ok && now.Sub(t) < wontQuarantineReemitInterval {
		return false
	}
	g.last[ip] = now
	return true
}

// clear forgets ip once it is back above the quarantine condition, so the
// next drop records a fresh conviction.
func (g *wontQuarantineGate) clear(ip string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.last, ip)
}

// PoolAgent manages IP assignment to this ISP's pool. Computes per-IP
// performance scores and moves IPs between ISP pool, warmup, and quarantine.
type PoolAgent struct {
	BaseAgent
	memory      *MemoryStore
	convictions *ConvictionStore
	alertCh     chan<- Decision
	wontGate    wontQuarantineGate // "WONT keep IP" (score < 50)
	reduceGate  wontQuarantineGate // "WONT allow full volume" (50 <= score < 70)
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

		if score >= 50 {
			a.wontGate.clear(ip)
		}
		if score < 50 || score >= 70 {
			a.reduceGate.clear(ip)
		}

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

			if a.convictions != nil && a.wontGate.allow(ip, now) {
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

			if a.convictions != nil && a.reduceGate.allow(ip, now) {
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
