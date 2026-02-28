package engine

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// SuppressionAgent is the "Highway Patrol" — zero-tolerance enforcement.
// Any recipient email associated with ANY negative signal gets permanently
// suppressed for this agent's ISP. One offense, never travels again.
type SuppressionAgent struct {
	BaseAgent
	store       *SuppressionStore
	memory      *MemoryStore
	convictions *ConvictionStore
	alertCh     chan<- Decision

	// Track transient failures for the 2-strike rule
	mu             sync.Mutex
	transientCount map[string][]time.Time // email -> timestamps

	// Velocity tracking for cross-agent alerts
	recentSuppressions []time.Time
}

// suppressionTriggerCategories are bounce categories that cause instant suppression.
var suppressionTriggerCategories = map[string]bool{
	"bad-mailbox":          true,
	"bad-domain":           true,
	"inactive-mailbox":     true,
	"no-answer-from-host":  true,
	"routing-errors":       true,
	"quota-issues":         true,
	"message-too-large":    true,
	"content-related":      true,
	"too-many-connections": true,
	"policy-related":       true,
	"spam-related":         true,
	"protocol-errors":      true,
	"relaying-issues":      true,
}

// NewSuppressionAgent creates a new ISP-scoped suppression agent.
func NewSuppressionAgent(id AgentID, config ISPConfig, store *SuppressionStore, memory *MemoryStore, convictions *ConvictionStore, alertCh chan<- Decision) *SuppressionAgent {
	return &SuppressionAgent{
		BaseAgent: BaseAgent{
			ID:     id,
			Config: config,
			Status: StatusActive,
		},
		store:          store,
		memory:         memory,
		convictions:    convictions,
		alertCh:        alertCh,
		transientCount: make(map[string][]time.Time),
	}
}

// ProcessRecord evaluates a single accounting record for suppression triggers.
func (a *SuppressionAgent) ProcessRecord(ctx context.Context, rec AccountingRecord) {
	if a.Status != StatusActive {
		return
	}

	email := strings.ToLower(rec.Recipient)
	if email == "" {
		return
	}

	// Already suppressed?
	if a.store.IsSuppressed(a.ID.ISP, email) {
		return
	}

	shouldSuppress := false
	reason := ""

	switch rec.Type {
	case "b": // bounce
		if suppressionTriggerCategories[rec.BounceCat] {
			shouldSuppress = true
			reason = rec.BounceCat
		}

	case "f": // FBL complaint
		shouldSuppress = true
		reason = "fbl-complaint"

	case "t": // transient failure
		if a.checkTransientThreshold(email) {
			shouldSuppress = true
			reason = "repeated-transient"
		}
	}

	if !shouldSuppress {
		return
	}

	now := time.Now()
	supp := Suppression{
		Email:        email,
		ISP:          a.ID.ISP,
		Reason:       reason,
		DSNCode:      rec.DSNStatus,
		DSNDiagnostic: rec.DSNDiag,
		SourceIP:     rec.SourceIP,
		SourceVMTA:   rec.VMTA,
		CampaignID:   rec.JobID,
		SuppressedAt: now,
	}

	isNew, err := a.store.Suppress(ctx, supp)
	if err != nil {
		log.Printf("[suppression/%s] error suppressing %s: %v", a.ID.ISP, email, err)
		return
	}
	if !isNew {
		return
	}

	log.Printf("[suppression/%s] SUPPRESSED %s reason=%s dsn=%s", a.ID.ISP, email, reason, rec.DSNStatus)

	// Track velocity
	a.mu.Lock()
	a.recentSuppressions = append(a.recentSuppressions, now)
	a.mu.Unlock()

	// Record conviction: "I WONT deliver to this email. Here's exactly why."
	if a.convictions != nil {
		tc := TemporalContext(now)
		statement := fmt.Sprintf(
			"I WONT deliver to %s via %s. %s with DSN %s from IP %s VMTA %s during campaign %s.",
			email, string(a.ID.ISP), reason, rec.DSNStatus, rec.SourceIP, rec.VMTA, rec.JobID,
		)
		a.convictions.Record(ctx, Conviction{
			AgentType: AgentSuppression,
			ISP:       a.ID.ISP,
			Verdict:   VerdictWont,
			Statement: statement,
			Context: MicroContext{
				Date:        tc.Date,
				DayOfWeek:   tc.DayOfWeek,
				HourUTC:     tc.HourUTC,
				IsHoliday:   tc.IsHoliday,
				HolidayName: tc.HolidayName,
				IP:          rec.SourceIP,
				VMTA:        rec.VMTA,
				Domain:      rec.Domain,
				Email:       email,
				CampaignID:  rec.JobID,
				Reason:      reason,
				DSNCodes:    []string{rec.DSNStatus},
				DSNDiagnostics: []string{rec.DSNDiag},
			},
			CreatedAt: now,
		})
	}

	// Emit decision for orchestrator
	decision := Decision{
		ISP:         a.ID.ISP,
		AgentType:   AgentSuppression,
		ActionTaken: "suppress_email",
		TargetType:  "email",
		TargetValue: email,
		Result:      "applied",
		CreatedAt:   now,
	}
	if a.alertCh != nil {
		select {
		case a.alertCh <- decision:
		default:
		}
	}

	// Check velocity thresholds for cross-agent alerts
	a.checkVelocity(ctx, now)
}

// checkTransientThreshold returns true if this email has 2+ transient
// failures within 24 hours.
func (a *SuppressionAgent) checkTransientThreshold(email string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-24 * time.Hour)

	timestamps := a.transientCount[email]
	// Prune old entries
	pruned := timestamps[:0]
	for _, t := range timestamps {
		if t.After(cutoff) {
			pruned = append(pruned, t)
		}
	}
	pruned = append(pruned, now)
	a.transientCount[email] = pruned

	return len(pruned) >= 2
}

// checkVelocity alerts other agents if suppression rate is abnormally high.
func (a *SuppressionAgent) checkVelocity(ctx context.Context, now time.Time) {
	a.mu.Lock()
	// Prune old suppressions
	cutoff5m := now.Add(-5 * time.Minute)
	pruned := a.recentSuppressions[:0]
	for _, t := range a.recentSuppressions {
		if t.After(cutoff5m) {
			pruned = append(pruned, t)
		}
	}
	a.recentSuppressions = pruned
	count5m := len(pruned)
	a.mu.Unlock()

	// > 100 suppressions in 5 min: alert reputation agent
	if count5m > 100 {
		log.Printf("[suppression/%s] VELOCITY ALERT: %d suppressions in 5min", a.ID.ISP, count5m)
		decision := Decision{
			ISP:         a.ID.ISP,
			AgentType:   AgentSuppression,
			ActionTaken: "velocity_alert_reputation",
			TargetType:  "isp",
			TargetValue: string(a.ID.ISP),
			Result:      "applied",
			CreatedAt:   now,
		}
		if a.alertCh != nil {
			select {
			case a.alertCh <- decision:
			default:
			}
		}
	}
}

// Evaluate satisfies the Agent interface. Suppression agent operates on individual
// records (via ProcessRecord), not on signal snapshots — this is a no-op.
func (a *SuppressionAgent) Evaluate(snap SignalSnapshot) []Decision {
	return nil
}

// PruneTransientCache periodically cleans up old transient failure records.
func (a *SuppressionAgent) PruneTransientCache() {
	a.mu.Lock()
	defer a.mu.Unlock()

	cutoff := time.Now().Add(-24 * time.Hour)
	for email, timestamps := range a.transientCount {
		pruned := timestamps[:0]
		for _, t := range timestamps {
			if t.After(cutoff) {
				pruned = append(pruned, t)
			}
		}
		if len(pruned) == 0 {
			delete(a.transientCount, email)
		} else {
			a.transientCount[email] = pruned
		}
	}
}
