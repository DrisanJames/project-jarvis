package engine

import (
	"sync"
	"time"
)

// BaseAgent provides common functionality shared by all 6 agent types.
type BaseAgent struct {
	ID     AgentID
	Config ISPConfig
	Status AgentStatus

	mu          sync.Mutex
	lastEvalAt  time.Time
	cooldownEnd time.Time
}

// SetStatus updates the agent status.
func (a *BaseAgent) SetStatus(s AgentStatus) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Status = s
}

// GetStatus returns the current agent status.
func (a *BaseAgent) GetStatus() AgentStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Status
}

// IsOnCooldown returns true if the agent is in a cooldown period.
func (a *BaseAgent) IsOnCooldown() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return time.Now().Before(a.cooldownEnd)
}

// SetCooldown puts the agent into a cooldown for the given duration.
func (a *BaseAgent) SetCooldown(d time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cooldownEnd = time.Now().Add(d)
	a.Status = StatusCooldown
}

// MarkEvaluated updates the last evaluation timestamp.
func (a *BaseAgent) MarkEvaluated() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastEvalAt = time.Now()
}

// Agent is the interface all agent types implement.
type Agent interface {
	GetID() AgentID
	GetStatus() AgentStatus
	SetStatus(AgentStatus)
	Evaluate(snapshot SignalSnapshot) []Decision
}

// GetID returns the agent's unique identifier.
func (a *BaseAgent) GetID() AgentID {
	return a.ID
}
