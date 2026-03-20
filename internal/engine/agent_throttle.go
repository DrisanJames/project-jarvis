package engine

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"math"
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
		s.LastBackoffAt.Equal(o.LastBackoffAt)
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

	lastPersisted ThrottleState // dirty-flag: skip persist when unchanged
}

// NewThrottleAgent creates a new ISP-scoped throttle agent.
func NewThrottleAgent(id AgentID, config ISPConfig, memory *MemoryStore, convictions *ConvictionStore, alertCh chan<- Decision) *ThrottleAgent {
	return &ThrottleAgent{
		BaseAgent: BaseAgent{
			ID:     id,
			Config: config,
			Status: StatusActive,
		},
		memory:         memory,
		convictions:    convictions,
		alertCh:        alertCh,
		currentRateAdj: 1.0,
		originalRate:   config.MaxMsgRate,
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

	currentEffective := int(float64(a.originalRate) * a.currentRateAdj)

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
		DeferralCount:   snap.Deferred5m,
		AcceptedCount:   snap.Sent5m - snap.Deferred5m,
		BounceCount:     snap.Bounced1h,
		DSNCodes:        snap.RecentDSNCodes,
		DSNDiagnostics:  snap.RecentDSNDiagnostics,
		EffectiveRate:   currentEffective,
		BackoffStep:     a.backoffCount,
		PriorRateAdj:    a.currentRateAdj,
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
		if a.inRecovery {
			a.currentRateAdj = a.lastStableRate
			a.inRecovery = false
			a.SetCooldown(30 * time.Minute)

			newRate := int(float64(a.originalRate) * a.currentRateAdj)
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

			newRate := int(float64(a.originalRate) * a.currentRateAdj)
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
		newRate := int(float64(a.originalRate) * newAdj)
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
		// Steady state: everything is good. Record a WILL conviction to remember
		// that this rate works under these conditions.
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
	return int(float64(a.originalRate) * a.currentRateAdj)
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
	a.lastPersisted = s
	effectiveRate := int(float64(a.originalRate) * a.currentRateAdj)
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
		CurrentRateAdj:  a.currentRateAdj,
		OriginalRate:    a.originalRate,
		LastStableRate:  a.lastStableRate,
		BackoffCount:    a.backoffCount,
		InRecovery:      a.inRecovery,
		RecoveryStarted: a.recoveryStarted,
		LastBackoffAt:   a.lastBackoffAt,
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
				 in_recovery, recovery_started, last_backoff_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW())
			ON CONFLICT (isp) DO UPDATE SET
				current_rate_adj = $2, original_rate = $3, last_stable_rate = $4,
				backoff_count = $5, in_recovery = $6, recovery_started = $7,
				last_backoff_at = $8, updated_at = NOW()
		`, isp, snap.CurrentRateAdj, snap.OriginalRate, snap.LastStableRate,
			snap.BackoffCount, snap.InRecovery, snap.RecoveryStarted, snap.LastBackoffAt)
		if err != nil {
			log.Printf("[throttle-state] persist %s failed: %v", isp, err)
		}
	}()
}
