package engine

import (
	"context"
	"time"
)

// DecisionStore persists governance decisions and queries recent history.
// The Orchestrator uses this instead of *sql.DB for all decision-related
// persistence, making it testable with in-memory or mock implementations.
type DecisionStore interface {
	PersistDecision(ctx context.Context, d Decision) error
	PersistAgentState(ctx context.Context, orgID string, isp ISP, agentType AgentType, status AgentStatus) error
	GetAgentStates(ctx context.Context, orgID string) ([]AgentState, error)
	GetISPAgentStates(ctx context.Context, orgID string, isp ISP) ([]AgentState, error)
	UpdateAgentStatus(ctx context.Context, orgID string, isp ISP, agentType AgentType, status AgentStatus) error
	QueryDecisions(ctx context.Context, orgID string, isp *ISP, agentType *AgentType, since *time.Time, limit int) ([]Decision, error)
	QueryIPWarmupState(ctx context.Context, orgID string, poolName string) (activeIPs, warmupIPs, quarantinedIPs, dailyCap int, err error)
}

// SignalStore persists computed signal snapshots and metric values.
// The SignalProcessor uses this to write rolling-window metrics to the database.
type SignalStore interface {
	PersistSignals(ctx context.Context, orgID string, snap SignalSnapshot, metrics []SignalMetric) error
}

// SignalMetric is a single metric row to persist, extracted from a snapshot.
type SignalMetric struct {
	ISP            ISP
	MetricName     string
	DimensionType  string
	DimensionValue string
	Value          float64
	WindowSeconds  int
}

// SuppressionRepository reads and writes ISP-scoped suppression entries.
// The SuppressionStore uses this instead of *sql.DB for all suppression
// data access, enabling mock-based testing.
type SuppressionRepository interface {
	LoadAll(ctx context.Context, orgID string) (map[ISP][]string, error)
	Add(ctx context.Context, s Suppression) error
	Remove(ctx context.Context, orgID string, isp ISP, email string) error
	Get(ctx context.Context, orgID string, isp ISP, email string) (*Suppression, error)
	List(ctx context.Context, orgID string, isp ISP, reason string, limit, offset int) ([]Suppression, int, error)
	Stats(ctx context.Context, orgID string, isp ISP) (*SuppressionStats, error)
	ListEmails(ctx context.Context, orgID string, isp ISP) ([]string, error)
}

// GlobalSuppressionWriter writes to the cross-ISP global suppression hub.
type GlobalSuppressionWriter interface {
	Suppress(ctx context.Context, email, reason, source, isp, dsnCode, dsnDiag, sourceIP, campaign string) error
	IsSuppressed(email string) bool
	Count() int
}

// ISPConfigStore reads and updates ISP configuration from the database.
type ISPConfigStore interface {
	LoadISPConfigs(ctx context.Context, orgID string) ([]ISPConfig, error)
	UpdateISPConfig(ctx context.Context, orgID string, cfg ISPConfig) error
}

// AlertSender sends governance alert notifications via email or other channels.
type AlertSender interface {
	SendDecisionAlert(d Decision)
	SendEmergencyAlert(isp ISP, message string)
}

// WorkerHealthReporter exposes health status for background workers.
// Used by the health check endpoint to report worker liveness.
type WorkerHealthReporter interface {
	IsHealthy() bool
	LastRunAt() time.Time
}
