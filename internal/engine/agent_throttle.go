package engine

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"math"
	"os"
	"strings"
	"sync"
	"time"
)

// ThrottleAgent is the "Traffic Controller" — the most dynamic agent.
// It manages sending rates per ISP using exponential backoff with 5% step-down.
// Unlike other agents that are strict enforcers, the throttle agent must be
// nuanced: it stores micro-memories of every rate adjustment with full context
// (time, holiday, volume, acceptance rate, DSN codes, recovery duration) and
// recalls similar past situations before deciding. This prevents it from being
// overly conservative — "Tuesday at 2pm is bad" is a macro generalization.
// Instead it remembers: "On Tuesday Dec 25 at 14:00 UTC, I sent 1000msg/s to
// Gmail, got 4 deferrals (421-4.7.28), 996 accepted (99.6%), true open rate
// 20% non-MPP."
// ThrottleState captures the full internal state of a ThrottleAgent for
// persistence and restoration. All fields are exported for DB scanning.
type ThrottleState struct {
	CurrentRateAdj  float64
	OriginalRate    int
	LastStableRate  float64
	BackoffCount    int
	InRecovery      bool
	RecoveryStarted time.Time
	LastBackoffAt   time.Time

	// Escalation state (Layer 2: engagement-driven rate increases above MaxMsgRate)
	EscalationAdj          float64   // 1.0 = baseline, >1.0 = escalated above MaxMsgRate
	EscalationCooldownUntil time.Time // next escalation allowed after this time
	LastEscalationAt       time.Time
}

// Equal compares two ThrottleState values using time.Equal for timestamps,
// avoiding false positives from monotonic clock or timezone metadata.
func (s ThrottleState) Equal(o ThrottleState) bool {
	return s.CurrentRateAdj == o.CurrentRateAdj &&
		s.OriginalRate == o.OriginalRate &&
		s.LastStableRate == o.LastStableRate &&
		s.BackoffCount == o.BackoffCount &&
		s.InRecovery == o.InRecovery &&
		s.RecoveryStarted.Equal(o.RecoveryStarted) &&
		s.LastBackoffAt.Equal(o.LastBackoffAt) &&
		s.EscalationAdj == o.EscalationAdj &&
		s.EscalationCooldownUntil.Equal(o.EscalationCooldownUntil) &&
		s.LastEscalationAt.Equal(o.LastEscalationAt)
}

type ThrottleAgent struct {
	BaseAgent
	memory       *MemoryStore
	convictions  *ConvictionStore
	alertCh      chan<- Decision
	rateRegistry *ISPRateRegistry
	db           *sql.DB
	shutdownCtx  context.Context // server lifecycle context for persist goroutines

	mu              sync.Mutex
	currentRateAdj  float64
	originalRate    int
	lastStableRate  float64
	backoffCount    int
	inRecovery      bool
	recoveryStarted time.Time
	lastBackoffAt   time.Time

	// Escalation state
	escalationAdj           float64
	escalationCooldownUntil time.Time
	lastEscalationAt        time.Time
	escalationEnabled       bool // cached from env at construction time

	lastPersisted ThrottleState
}

// NewThrottleAgent creates a new ISP-scoped throttle agent.
func NewThrottleAgent(id AgentID, config ISPConfig, memory *MemoryStore, convictions *ConvictionStore, alertCh chan<- Decision) *ThrottleAgent {
	return &ThrottleAgent{
		BaseAgent: BaseAgent{
			ID:     id,
			Config: config,
			Status: StatusActive,
		},
		memory:            memory,
		convictions:       convictions,
		alertCh:           alertCh,
		currentRateAdj:    1.0,
		originalRate:      config.MaxMsgRate,
		escalationAdj:     1.0,
		escalationEnabled: os.Getenv("ENABLE_ENGAGEMENT_ESCALATION") == "true",
	}
}

// Evaluate checks deferral rates and adjusts sending rates.
// Before each decision, it recalls similar past situations from its conviction
// memory to inform whether to be aggressive or conservative.
func (a *ThrottleAgent) Evaluate(snap SignalSnapshot) []Decision {
	if a.GetStatus() != StatusActive || a.IsOnCooldown() {
		return nil
	}
	a.MarkEvaluated()

	var decisions []Decision
	now := time.Now()
	tc := TemporalContext(now)
	ctx := context.Background()

	deferralRate := snap.DeferralRate5m
	acceptanceRate := 0.0
	if snap.Sent5m > 0 {
		acceptanceRate = float64(snap.Sent5m-snap.Deferred5m) / float64(snap.Sent5m) * 100
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	currentEffective := a.computeEffectiveRateLocked()

	// Build the current situation context for recall and conviction recording
	situationCtx := MicroContext{
		Date: tc.Date, DayOfWeek: tc.DayOfWeek, HourUTC: tc.HourUTC,
		IsHoliday: tc.IsHoliday, HolidayName: tc.HolidayName,
		Pool:            a.Config.PoolName,
		AttemptedRate:   currentEffective,
		AttemptedVolume: snap.Sent5m,
		BounceRate:      snap.BounceRate5m,
		DeferralRate:    deferralRate,
		ComplaintRate:   snap.ComplaintRate1h,
		AcceptanceRate:  acceptanceRate,
		TrueOpenRate:    snap.TrueOpenRate1h,
		DeferralCount:   snap.Deferred5m,
		AcceptedCount:   snap.Sent5m - snap.Deferred5m,
		BounceCount:     snap.Bounced1h,
		DSNCodes:        snap.RecentDSNCodes,
		DSNDiagnostics:  snap.RecentDSNDiagnostics,
		EffectiveRate:   currentEffective,
		BackoffStep:     a.backoffCount,
		PriorRateAdj:    a.currentRateAdj,

		OpenRate1h:            snap.OpenRate1h,
		ClickRate1h:           snap.ClickRate1h,
		UniqueClicks:          snap.UniqueClicks1h,
		ClickToComplaintRatio: snap.ClickToComplaintRatio1h,
		EngagementScore:       snap.EngagementScore1h,
		EscalationAdj:         a.escalationAdj,
	}

	// Recall similar past situations to inform the decision
	var priorWisdom string
	if a.convictions != nil {
		similar := a.convictions.RecallSimilar(a.ID.ISP, AgentThrottle, situationCtx, 3)
		if len(similar) > 0 {
			priorWisdom = a.synthesizePriorWisdom(similar)
		}
	}

	if deferralRate > 20 {
		// Any escalation is immediately reverted on deferral spike
		if a.escalationAdj > 1.0 {
			a.escalationAdj = 1.0
			log.Printf("[throttle:%s] escalation reverted due to deferral spike (%.1f%%)", a.ID.ISP, deferralRate)
		}

		if a.inRecovery {
			a.currentRateAdj = a.lastStableRate
			a.inRecovery = false
			a.SetCooldown(30 * time.Minute)

			newRate := a.computeEffectiveRateLocked()
			if a.rateRegistry != nil {
				a.rateRegistry.SetRate(a.ID.ISP, float64(newRate))
			}
			decisions = append(decisions, Decision{
				ISP:         a.ID.ISP,
				AgentType:   AgentThrottle,
				ActionTaken: "snap_to_stable_rate",
				ActionParams: mustJSON(map[string]interface{}{
					"rate_adj":       a.currentRateAdj,
					"effective_rate": newRate,
					"deferral_rate":  deferralRate,
				}),
				TargetType:  "isp",
				TargetValue: string(a.ID.ISP),
				Result:      "applied",
				CreatedAt:   now,
			})

			if a.convictions != nil {
				recoveryDuration := now.Sub(a.recoveryStarted).Minutes()
				situationCtx.EffectiveRate = newRate
				situationCtx.RecoveryTimeMin = recoveryDuration
				a.convictions.Record(ctx, Conviction{
					AgentType: AgentThrottle,
					ISP:       a.ID.ISP,
					Verdict:   VerdictWont,
					Statement: fmt.Sprintf(
						"I WONT continue recovery to %s at this pace. Deferrals resurged to %.1f%% during recovery attempt (%.0fmin in). "+
						"Snapped back to stable rate %d/hr (adj %.2f). %d sent in 5min window, %d deferred, %d accepted (%.1f%%). DSN: %s. %s",
						a.ID.ISP, deferralRate, recoveryDuration, newRate, a.currentRateAdj,
						snap.Sent5m, snap.Deferred5m, snap.Sent5m-snap.Deferred5m, acceptanceRate,
						strings.Join(snap.RecentDSNCodes, ", "), priorWisdom,
					),
					Context:   situationCtx,
					CreatedAt: now,
				})
			}

		} else {
			a.backoffCount++
			reduction := math.Pow(0.95, float64(a.backoffCount))
			a.currentRateAdj = reduction
			a.lastBackoffAt = now

			newRate := a.computeEffectiveRateLocked()
			if a.rateRegistry != nil {
				a.rateRegistry.SetRate(a.ID.ISP, float64(newRate))
			}
			action := "reduce_rate"

			if deferralRate > 40 {
				action = "backoff_mode"
			}

			decisions = append(decisions, Decision{
				ISP:         a.ID.ISP,
				AgentType:   AgentThrottle,
				ActionTaken: action,
				ActionParams: mustJSON(map[string]interface{}{
					"rate_adj":       a.currentRateAdj,
					"effective_rate": newRate,
					"backoff_step":   a.backoffCount,
					"deferral_rate":  deferralRate,
				}),
				TargetType:  "isp",
				TargetValue: string(a.ID.ISP),
				Result:      "applied",
				CreatedAt:   now,
			})

			if a.convictions != nil {
				situationCtx.EffectiveRate = newRate
				situationCtx.BackoffStep = a.backoffCount
				severity := "elevated"
				if deferralRate > 40 {
					severity = "critical"
				}
				a.convictions.Record(ctx, Conviction{
					AgentType: AgentThrottle,
					ISP:       a.ID.ISP,
					Verdict:   VerdictWont,
					Statement: fmt.Sprintf(
						"I WONT send at %d/hr to %s. Deferral rate %s at %.1f%% (step %d backoff). "+
						"Reduced from %d/hr to %d/hr (adj %.3f). %d sent in 5min, %d deferred, %d accepted (%.1f%%). DSN: %s. %s",
						currentEffective, a.ID.ISP, severity, deferralRate, a.backoffCount,
						currentEffective, newRate, a.currentRateAdj,
						snap.Sent5m, snap.Deferred5m, snap.Sent5m-snap.Deferred5m, acceptanceRate,
						strings.Join(snap.RecentDSNCodes, ", "), priorWisdom,
					),
					Context:   situationCtx,
					CreatedAt: now,
				})
			}
		}
	} else if deferralRate < 10 && a.currentRateAdj < 1.0 {
		if !a.inRecovery {
			a.lastStableRate = a.currentRateAdj
			a.inRecovery = true
			a.recoveryStarted = now
		}

		newAdj := math.Min(a.currentRateAdj*1.10, 1.0)
		a.currentRateAdj = newAdj
		newRate := a.computeEffectiveRateLocked()
		if a.rateRegistry != nil {
			a.rateRegistry.SetRate(a.ID.ISP, float64(newRate))
		}

		if newAdj >= 1.0 {
			a.backoffCount = 0
			a.inRecovery = false
		}

		decisions = append(decisions, Decision{
			ISP:         a.ID.ISP,
			AgentType:   AgentThrottle,
			ActionTaken: "increase_rate",
			ActionParams: mustJSON(map[string]interface{}{
				"rate_adj":       a.currentRateAdj,
				"effective_rate": newRate,
				"deferral_rate":  deferralRate,
				"recovering":     a.inRecovery,
			}),
			TargetType:  "isp",
			TargetValue: string(a.ID.ISP),
			Result:      "applied",
			CreatedAt:   now,
		})

		if a.convictions != nil {
			situationCtx.EffectiveRate = newRate
			recoveryDuration := 0.0
			if !a.recoveryStarted.IsZero() {
				recoveryDuration = now.Sub(a.recoveryStarted).Minutes()
			}
			situationCtx.RecoveryTimeMin = recoveryDuration

			statusNote := "recovering"
			if newAdj >= 1.0 {
				statusNote = "fully recovered"
			}
			a.convictions.Record(ctx, Conviction{
				AgentType: AgentThrottle,
				ISP:       a.ID.ISP,
				Verdict:   VerdictWill,
				Statement: fmt.Sprintf(
					"I WILL increase rate to %d/hr for %s (%s). Deferrals dropped to %.1f%%, "+
					"recovery from %d/hr → %d/hr over %.0fmin. %d sent in 5min, %d deferred, %d accepted (%.1f%%). "+
					"DSN: %s. %s",
					newRate, a.ID.ISP, statusNote, deferralRate,
					int(float64(a.originalRate)*a.lastStableRate), newRate, recoveryDuration,
					snap.Sent5m, snap.Deferred5m, snap.Sent5m-snap.Deferred5m, acceptanceRate,
					strings.Join(snap.RecentDSNCodes, ", "), priorWisdom,
				),
				Context:   situationCtx,
				CreatedAt: now,
			})
		}
	} else if deferralRate <= 5 && a.currentRateAdj >= 1.0 && snap.Sent5m >= 50 {
		// Steady state: everything is good.
		escalated := a.tryEngagementEscalation(ctx, snap, now, currentEffective, deferralRate, acceptanceRate, &situationCtx, priorWisdom, &decisions)

		if !escalated {
			if a.convictions != nil {
				a.convictions.Record(ctx, Conviction{
					AgentType: AgentThrottle,
					ISP:       a.ID.ISP,
					Verdict:   VerdictWill,
					Statement: fmt.Sprintf(
						"I WILL send at %d/hr to %s. Steady state, deferral rate %.1f%%. "+
						"%d sent in 5min, %d deferred, %d accepted (%.1f%%). No action needed. %s",
						currentEffective, a.ID.ISP, deferralRate,
						snap.Sent5m, snap.Deferred5m, snap.Sent5m-snap.Deferred5m, acceptanceRate,
						priorWisdom,
					),
					Context:   situationCtx,
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

	if len(decisions) > 0 {
		a.persistState()
	}

	return decisions
}

// synthesizePriorWisdom builds a brief note from similar past convictions.
// This gets embedded in the new conviction's statement so the agent's reasoning
// chain is visible: "Last time I was in this situation, I decided X."
func (a *ThrottleAgent) synthesizePriorWisdom(similar []ScoredConviction) string {
	if len(similar) == 0 {
		return ""
	}
	var parts []string
	for i, sc := range similar {
		if i >= 2 {
			break
		}
		parts = append(parts, fmt.Sprintf(
			"[Prior %s on %s (sim=%.0f%%): %s at rate %d/hr, deferral %.1f%%, acceptance %.1f%%]",
			strings.ToUpper(string(sc.Conviction.Verdict)),
			sc.Conviction.Context.Date,
			sc.Similarity*100,
			sc.Conviction.Context.DayOfWeek,
			sc.Conviction.Context.EffectiveRate,
			sc.Conviction.Context.DeferralRate,
			sc.Conviction.Context.AcceptanceRate,
		))
	}
	return strings.Join(parts, " ")
}

// MatchesDeferralCode checks if a DSN diagnostic matches this ISP's known deferral codes.
func (a *ThrottleAgent) MatchesDeferralCode(diagnostic string) bool {
	diag := strings.ToLower(diagnostic)
	for _, code := range a.Config.DeferralCodes {
		if strings.Contains(diag, strings.ToLower(code)) {
			return true
		}
	}
	return false
}

// SetRateRegistry injects the shared rate registry so Evaluate can
// push rate changes to the application-level limiter.
func (a *ThrottleAgent) SetRateRegistry(r *ISPRateRegistry) {
	a.rateRegistry = r
}

// GetEffectiveRate returns the current adjusted sending rate.
func (a *ThrottleAgent) GetEffectiveRate() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.computeEffectiveRateLocked()
}

// computeEffectiveRateLocked returns originalRate * rateAdj * escalationAdj.
// Caller must hold mu.
func (a *ThrottleAgent) computeEffectiveRateLocked() int {
	return int(float64(a.originalRate) * a.currentRateAdj * a.escalationAdj)
}

// maxEscalationMultiplier returns the configured ceiling for escalation.
// Defaults to 1.0 (no escalation) if not configured.
func (a *ThrottleAgent) maxEscalationMultiplier() float64 {
	if a.Config.MaxEscalationMultiplier > 1.0 {
		return a.Config.MaxEscalationMultiplier
	}
	return 1.5 // safe default when not explicitly configured
}

// SetDB attaches a database handle for state persistence.
func (a *ThrottleAgent) SetDB(db *sql.DB) {
	a.db = db
}

// SetShutdownContext provides the server lifecycle context so persist
// goroutines are cancelled on shutdown rather than orphaned.
func (a *ThrottleAgent) SetShutdownContext(ctx context.Context) {
	a.shutdownCtx = ctx
}

// RestoreState atomically restores all internal fields from a previously
// persisted snapshot. Called on startup before the first Evaluate cycle.
// After restoring, pushes the effective rate to the registry so the limiter
// reflects the throttled state immediately (not just after the next signal).
func (a *ThrottleAgent) RestoreState(s ThrottleState) {
	a.mu.Lock()
	if s.OriginalRate > 0 {
		a.originalRate = s.OriginalRate
	}
	a.currentRateAdj = s.CurrentRateAdj
	a.lastStableRate = s.LastStableRate
	a.backoffCount = s.BackoffCount
	a.inRecovery = s.InRecovery
	a.recoveryStarted = s.RecoveryStarted
	a.lastBackoffAt = s.LastBackoffAt
	if s.EscalationAdj > 0 {
		a.escalationAdj = s.EscalationAdj
	} else {
		a.escalationAdj = 1.0
	}
	a.escalationCooldownUntil = s.EscalationCooldownUntil
	a.lastEscalationAt = s.LastEscalationAt
	a.lastPersisted = s
	effectiveRate := a.computeEffectiveRateLocked()
	a.mu.Unlock()

	if a.rateRegistry != nil && effectiveRate > 0 {
		a.rateRegistry.SetRate(a.ID.ISP, float64(effectiveRate))
		log.Printf("[throttle:%s] restored rate pushed to registry: %d msgs/hr (adj=%.3f)",
			a.ID.ISP, effectiveRate, s.CurrentRateAdj)
	}
}

// GetState returns a snapshot of the agent's current internal state.
func (a *ThrottleAgent) GetState() ThrottleState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.snapshotStateLocked()
}

// snapshotStateLocked captures current state. Caller must hold mu.
func (a *ThrottleAgent) snapshotStateLocked() ThrottleState {
	return ThrottleState{
		CurrentRateAdj:          a.currentRateAdj,
		OriginalRate:            a.originalRate,
		LastStableRate:          a.lastStableRate,
		BackoffCount:            a.backoffCount,
		InRecovery:              a.inRecovery,
		RecoveryStarted:         a.recoveryStarted,
		LastBackoffAt:           a.lastBackoffAt,
		EscalationAdj:           a.escalationAdj,
		EscalationCooldownUntil: a.escalationCooldownUntil,
		LastEscalationAt:        a.lastEscalationAt,
	}
}

// persistState writes the current internal state to the database if it has
// changed since the last persist (dirty-flag check). Fire-and-forget with
// a 3-second timeout so Evaluate is never blocked on I/O.
func (a *ThrottleAgent) persistState() {
	if a.db == nil {
		return
	}
	snap := a.snapshotStateLocked()
	if snap.Equal(a.lastPersisted) {
		return
	}
	a.lastPersisted = snap
	isp := string(a.ID.ISP)
	parentCtx := a.shutdownCtx
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	go func() {
		ctx, cancel := context.WithTimeout(parentCtx, 3*time.Second)
		defer cancel()
		_, err := a.db.ExecContext(ctx, `
			INSERT INTO mailing_engine_throttle_agent_state
				(isp, current_rate_adj, original_rate, last_stable_rate, backoff_count,
				 in_recovery, recovery_started, last_backoff_at,
				 escalation_adj, escalation_cooldown_until, last_escalation_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, NOW())
			ON CONFLICT (isp) DO UPDATE SET
				current_rate_adj = $2, original_rate = $3, last_stable_rate = $4,
				backoff_count = $5, in_recovery = $6, recovery_started = $7,
				last_backoff_at = $8, escalation_adj = $9, escalation_cooldown_until = $10,
				last_escalation_at = $11, updated_at = NOW()
		`, isp, snap.CurrentRateAdj, snap.OriginalRate, snap.LastStableRate,
			snap.BackoffCount, snap.InRecovery, snap.RecoveryStarted, snap.LastBackoffAt,
			snap.EscalationAdj, snap.EscalationCooldownUntil, snap.LastEscalationAt)
		if err != nil {
			log.Printf("[throttle-state] persist %s failed: %v", isp, err)
		}
	}()
}

// ---------------------------------------------------------------------------
// Layer 2: Engagement-Driven Escalation
// ---------------------------------------------------------------------------

const (
	escalationStepPct      = 0.05          // 5% per escalation step
	escalationCooldown     = 4 * time.Hour // minimum wait between escalation steps
	escalationMinClicks    = 100           // require 100+ unique clicks/hr for confidence
	escalationMinCTCRatio  = 10.0          // clicks-to-complaints ratio ≥ 10:1
	escalationMinSent      = 200           // require meaningful sending volume
	escalationRevertWindow = 2 * time.Hour // auto-revert if engagement drops within this window
)

// tryEngagementEscalation checks whether engagement signals justify a proactive
// rate increase above MaxMsgRate. Returns true if an escalation decision was made.
// Caller must hold mu. This is a no-op if the feature flag is off.
func (a *ThrottleAgent) tryEngagementEscalation(
	ctx context.Context,
	snap SignalSnapshot,
	now time.Time,
	currentEffective int,
	deferralRate, acceptanceRate float64,
	situationCtx *MicroContext,
	priorWisdom string,
	decisions *[]Decision,
) bool {
	if !a.escalationEnabled {
		return false
	}

	maxMult := a.maxEscalationMultiplier()
	if maxMult <= 1.0 {
		return false
	}

	// Check auto-revert: if currently escalated but engagement has degraded, revert.
	// Guard: don't revert when Redis data is insufficient (hour-boundary key rotation
	// or Redis outage produces zero metrics — that's not degraded engagement, it's
	// missing data). Complaints5m > 5 is checked unconditionally since that's a
	// direct negative signal independent of hourly counters.
	if a.escalationAdj > 1.0 {
		hasEngagementData := snap.Sent1h >= escalationMinSent && snap.UniqueClicks1h > 0
		complaintsSpike := snap.Complaints5m > 5
		engagementDegraded := hasEngagementData &&
			(snap.ClickToComplaintRatio1h < escalationMinCTCRatio ||
				snap.UniqueClicks1h < escalationMinClicks/2)

		if complaintsSpike || engagementDegraded {

			prevAdj := a.escalationAdj
			a.escalationAdj = 1.0
			newRate := a.computeEffectiveRateLocked()
			if a.rateRegistry != nil {
				a.rateRegistry.SetRate(a.ID.ISP, float64(newRate))
			}

			*decisions = append(*decisions, Decision{
				ISP:         a.ID.ISP,
				AgentType:   AgentThrottle,
				ActionTaken: "revert_escalation",
				ActionParams: mustJSON(map[string]interface{}{
					"prev_escalation_adj":       prevAdj,
					"effective_rate":             newRate,
					"click_to_complaint_ratio":   snap.ClickToComplaintRatio1h,
					"unique_clicks":              snap.UniqueClicks1h,
					"complaints_5m":              snap.Complaints5m,
					"engagement_score":           snap.EngagementScore1h,
				}),
				TargetType:  "isp",
				TargetValue: string(a.ID.ISP),
				Result:      "applied",
				CreatedAt:   now,
			})

			if a.convictions != nil {
				situationCtx.EffectiveRate = newRate
				a.convictions.Record(ctx, Conviction{
					AgentType: AgentThrottle,
					ISP:       a.ID.ISP,
					Verdict:   VerdictWont,
					Statement: fmt.Sprintf(
						"I WONT maintain escalated rate for %s. Engagement degraded: CTC ratio %.1f (need %.0f), "+
						"unique clicks %d, 5m complaints %d. Reverted escalation from %.0f%% to baseline. Rate %d/hr. %s",
						a.ID.ISP, snap.ClickToComplaintRatio1h, escalationMinCTCRatio,
						snap.UniqueClicks1h, snap.Complaints5m, (prevAdj-1)*100, newRate, priorWisdom,
					),
					Context:   *situationCtx,
					CreatedAt: now,
				})
			}

			log.Printf("[throttle:%s] escalation reverted: CTC=%.1f clicks=%d complaints5m=%d",
				a.ID.ISP, snap.ClickToComplaintRatio1h, snap.UniqueClicks1h, snap.Complaints5m)
			return true
		}
	}

	// Escalation preconditions
	if now.Before(a.escalationCooldownUntil) {
		return false
	}
	if a.escalationAdj >= maxMult {
		return false
	}
	if snap.Sent1h < escalationMinSent {
		return false
	}
	if snap.UniqueClicks1h < escalationMinClicks {
		return false
	}
	if snap.ClickToComplaintRatio1h < escalationMinCTCRatio {
		return false
	}
	if snap.BounceRate1h > 2.0 {
		return false
	}
	if snap.ComplaintRate1h > 0.1 {
		return false
	}

	// All preconditions met — escalate by one step
	newEscAdj := math.Min(a.escalationAdj+escalationStepPct, maxMult)
	a.escalationAdj = newEscAdj
	a.escalationCooldownUntil = now.Add(escalationCooldown)
	a.lastEscalationAt = now

	newRate := a.computeEffectiveRateLocked()
	if a.rateRegistry != nil {
		a.rateRegistry.SetRate(a.ID.ISP, float64(newRate))
	}

	*decisions = append(*decisions, Decision{
		ISP:         a.ID.ISP,
		AgentType:   AgentThrottle,
		ActionTaken: "escalate_rate",
		ActionParams: mustJSON(map[string]interface{}{
			"escalation_adj":            newEscAdj,
			"effective_rate":             newRate,
			"max_msg_rate":               a.originalRate,
			"click_to_complaint_ratio":   snap.ClickToComplaintRatio1h,
			"unique_clicks":              snap.UniqueClicks1h,
			"engagement_score":           snap.EngagementScore1h,
			"open_rate_1h":               snap.OpenRate1h,
			"true_open_rate_1h":          snap.TrueOpenRate1h,
			"click_rate_1h":              snap.ClickRate1h,
			"max_escalation_multiplier":  maxMult,
		}),
		TargetType:  "isp",
		TargetValue: string(a.ID.ISP),
		Result:      "applied",
		CreatedAt:   now,
	})

	if a.convictions != nil {
		situationCtx.EffectiveRate = newRate
		a.convictions.Record(ctx, Conviction{
			AgentType: AgentThrottle,
			ISP:       a.ID.ISP,
			Verdict:   VerdictWill,
			Statement: fmt.Sprintf(
				"I WILL escalate rate to %d/hr for %s (%.0f%% above base %d/hr). "+
				"Engagement signals strong: CTC ratio %.1f, %d unique clicks, open rate %.1f%%, click rate %.1f%%, "+
				"engagement score %.1f. Deferrals %.1f%%, bounces %.1f%%, complaints %.2f%%. "+
				"Next escalation available in %s. %s",
				newRate, a.ID.ISP, (newEscAdj-1)*100, a.originalRate,
				snap.ClickToComplaintRatio1h, snap.UniqueClicks1h, snap.OpenRate1h, snap.ClickRate1h,
				snap.EngagementScore1h, deferralRate, snap.BounceRate1h, snap.ComplaintRate1h,
				escalationCooldown, priorWisdom,
			),
			Context:   *situationCtx,
			CreatedAt: now,
		})
	}

	log.Printf("[throttle:%s] ESCALATED to %d/hr (%.0f%% above base %d/hr, CTC=%.1f, clicks=%d)",
		a.ID.ISP, newRate, (newEscAdj-1)*100, a.originalRate, snap.ClickToComplaintRatio1h, snap.UniqueClicks1h)
	return true
}
