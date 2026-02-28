package engine

import (
	"context"
	"encoding/json"
	"log"
	"sync"
)

// Orchestrator coordinates all 48 agents across 8 ISPs. It receives signal
// snapshots, fans them to ISP agent clusters, collects decisions, prevents
// conflicts, and dispatches actions to the executor.
type Orchestrator struct {
	decisions DecisionStore
	orgID     string
	factory   *AgentFactory
	processor *SignalProcessor
	ingestor  *Ingestor
	executor  *Executor
	alerter   *Alerter
	memory    *MemoryStore
	store     *SuppressionStore

	mu             sync.Mutex
	recentDecisions []Decision
	running        bool
	cancelFn       context.CancelFunc
}

// NewOrchestrator creates the global orchestrator.
func NewOrchestrator(
	decisions DecisionStore,
	orgID string,
	factory *AgentFactory,
	processor *SignalProcessor,
	ingestor *Ingestor,
	executor *Executor,
	alerter *Alerter,
	memory *MemoryStore,
	store *SuppressionStore,
) *Orchestrator {
	return &Orchestrator{
		decisions: decisions,
		orgID:     orgID,
		factory:   factory,
		processor: processor,
		ingestor:  ingestor,
		executor:  executor,
		alerter:   alerter,
		memory:    memory,
		store:     store,
	}
}

// Start begins the orchestration loops.
func (o *Orchestrator) Start(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	o.cancelFn = cancel
	o.running = true

	// Subscribe ingestor records to suppression agents
	for _, isp := range AllISPs() {
		ch := make(chan AccountingRecord, 5000)
		o.ingestor.SubscribeISP(isp, ch)
		go o.runSuppressionListener(ctx, isp, ch)
	}

	// Subscribe to signal snapshots for agent evaluation
	signalCh := make(chan SignalSnapshot, 100)
	o.processor.Subscribe(signalCh)
	go o.runAgentEvalLoop(ctx, signalCh)

	// Process agent decisions
	go o.runDecisionProcessor(ctx)

	// Start signal processor
	o.processor.Start(ctx)

	// Start ingestor polling
	o.ingestor.StartPolling(ctx)

	// Start executor reload loop
	o.executor.StartReloadLoop(ctx)

	// Start suppression file sync
	o.store.StartFileSync(ctx)

	log.Println("[orchestrator] started — 48 agents active across 8 ISPs")
}

// Stop halts the orchestrator.
func (o *Orchestrator) Stop() {
	if o.cancelFn != nil {
		o.cancelFn()
	}
	o.running = false
	if o.memory != nil {
		o.memory.Stop()
	}
	o.store.Stop()
}

// IsRunning returns whether the orchestrator is active.
func (o *Orchestrator) IsRunning() bool {
	return o.running
}

func (o *Orchestrator) runSuppressionListener(ctx context.Context, isp ISP, ch <-chan AccountingRecord) {
	agent := o.factory.GetSuppressionAgent(isp)
	if agent == nil {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case rec := <-ch:
			agent.ProcessRecord(ctx, rec)
		}
	}
}

func (o *Orchestrator) runAgentEvalLoop(ctx context.Context, signalCh <-chan SignalSnapshot) {
	for {
		select {
		case <-ctx.Done():
			return
		case snap := <-signalCh:
			o.evaluateISPAgents(snap)
		}
	}
}

func (o *Orchestrator) evaluateISPAgents(snap SignalSnapshot) {
	agents := o.factory.GetISPAgents(snap.ISP)
	for _, agent := range agents {
		if agent.GetStatus() == StatusPaused {
			continue
		}
		agent.Evaluate(snap)
	}
}

func (o *Orchestrator) runDecisionProcessor(ctx context.Context) {
	alertCh := o.factory.AlertChannel()

	for {
		select {
		case <-ctx.Done():
			return
		case decision := <-alertCh:
			o.processDecision(ctx, decision)
		}
	}
}

func (o *Orchestrator) processDecision(ctx context.Context, d Decision) {
	d.OrganizationID = o.orgID
	if d.SignalValues == nil {
		d.SignalValues = json.RawMessage("{}")
	}
	if d.ActionParams == nil {
		d.ActionParams = json.RawMessage("{}")
	}

	if err := o.decisions.PersistDecision(ctx, d); err != nil {
		log.Printf("[orchestrator] persist decision error: %v", err)
	}

	// Update agent state in DB
	o.updateAgentState(ctx, d.ISP, d.AgentType)

	// Track recent decisions
	o.mu.Lock()
	o.recentDecisions = append(o.recentDecisions, d)
	if len(o.recentDecisions) > 200 {
		o.recentDecisions = o.recentDecisions[len(o.recentDecisions)-200:]
	}
	o.mu.Unlock()

	// Execute PMTA command if action is pending
	if d.Result == "pending" {
		if err := o.executor.Execute(ctx, d); err != nil {
			log.Printf("[orchestrator] execute error: %v", err)
		}
	}

	// Alert on significant actions
	switch d.ActionTaken {
	case "emergency_halt":
		if o.alerter != nil {
			incident := IncidentReport{
				ISP:         d.ISP,
				Trigger:     "emergency_halt",
				DetectedAt:  d.CreatedAt,
				AffectedIPs: []string{d.TargetValue},
				Status:      "active",
			}
			o.alerter.SendEmergencyAlert(incident)
		}
	case "disable_source_ip", "quarantine_ip", "pause_isp_queues":
		if o.alerter != nil {
			o.alerter.SendDecisionAlert(d)
		}
	case "velocity_alert_reputation":
		if o.alerter != nil {
			o.alerter.SendVelocityAlert(d.ISP, 100, 100)
		}
	}

	// Write to S3 memory
	if o.memory != nil {
		o.memory.AppendDecision(ctx, d.ISP, d.AgentType, d)
	}
}

func (o *Orchestrator) updateAgentState(ctx context.Context, isp ISP, agentType AgentType) {
	agent := o.factory.GetAgent(isp, agentType)
	if agent == nil {
		return
	}

	status := agent.GetStatus()
	if err := o.decisions.PersistAgentState(ctx, o.orgID, isp, agentType, status); err != nil {
		log.Printf("[orchestrator] update agent state error: %v", err)
	}
}

// GetRecentDecisions returns the most recent decisions.
func (o *Orchestrator) GetRecentDecisions(limit int) []Decision {
	o.mu.Lock()
	defer o.mu.Unlock()
	if limit <= 0 || limit > len(o.recentDecisions) {
		limit = len(o.recentDecisions)
	}
	start := len(o.recentDecisions) - limit
	result := make([]Decision, limit)
	copy(result, o.recentDecisions[start:])
	return result
}

// GetAgentStates returns all agent states from the database.
func (o *Orchestrator) GetAgentStates(ctx context.Context) ([]AgentState, error) {
	return o.decisions.GetAgentStates(ctx, o.orgID)
}

// GetISPAgentStates returns agent states for a specific ISP.
func (o *Orchestrator) GetISPAgentStates(ctx context.Context, isp ISP) ([]AgentState, error) {
	return o.decisions.GetISPAgentStates(ctx, o.orgID, isp)
}

// PauseAgent pauses a specific agent.
func (o *Orchestrator) PauseAgent(ctx context.Context, isp ISP, agentType AgentType) error {
	agent := o.factory.GetAgent(isp, agentType)
	if agent == nil {
		return nil
	}
	agent.SetStatus(StatusPaused)
	return o.decisions.UpdateAgentStatus(ctx, o.orgID, isp, agentType, StatusPaused)
}

// ResumeAgent resumes a specific agent.
func (o *Orchestrator) ResumeAgent(ctx context.Context, isp ISP, agentType AgentType) error {
	agent := o.factory.GetAgent(isp, agentType)
	if agent == nil {
		return nil
	}
	agent.SetStatus(StatusActive)
	return o.decisions.UpdateAgentStatus(ctx, o.orgID, isp, agentType, StatusActive)
}

// Override handles manual override commands.
func (o *Orchestrator) Override(ctx context.Context, action string, params map[string]string) error {
	switch action {
	case "resume_all":
		for _, isp := range AllISPs() {
			for _, at := range AllAgentTypes() {
				o.ResumeAgent(ctx, isp, at)
			}
		}
		return o.executor.ResumeAll(ctx)
	case "resume_isp":
		isp := ISP(params["isp"])
		for _, at := range AllAgentTypes() {
			o.ResumeAgent(ctx, isp, at)
		}
		return o.executor.ResumeISP(ctx, isp)
	default:
		return nil
	}
}

// GetCampaignReadiness aggregates per-ISP health, warmup state, and throughput
// capacity for the campaign wizard's readiness step.
func (o *Orchestrator) GetCampaignReadiness(ctx context.Context) CampaignReadinessResponse {
	states, _ := o.GetAgentStates(ctx)
	decisions := o.GetRecentDecisions(100)

	var isps []ISPReadiness
	totalCap := 0
	overallBlocked := false

	for _, isp := range AllISPs() {
		ispStates := filterStates(states, isp)
		snap := o.processor.GetSnapshot(isp)

		health := 100.0 - (snap.BounceRate1h * 10) - (snap.ComplaintRate1h * 100) - (snap.DeferralRate5m * 2)
		if health < 0 {
			health = 0
		}

		activeAgents := 0
		hasEmergency := false
		for _, s := range ispStates {
			if s.Status == StatusActive || s.Status == StatusFiring {
				activeAgents++
			}
			if s.Status == StatusFiring {
				hasEmergency = true
			}
		}

		activeIPs, warmupIPs, quarantinedIPs, dailyCap, _ := o.decisions.QueryIPWarmupState(ctx, o.orgID, PoolNameForISP(isp))

		status := "ready"
		var warnings []string
		if hasEmergency {
			status = "blocked"
			overallBlocked = true
			warnings = append(warnings, "Emergency halt active for this ISP")
		} else if health < 60 || warmupIPs > activeIPs {
			status = "caution"
			if health < 60 {
				warnings = append(warnings, "Health score below 60")
			}
			if warmupIPs > activeIPs {
				warnings = append(warnings, "More IPs warming up than active")
			}
		}

		// Check recent emergency/quarantine decisions
		for _, d := range decisions {
			if d.ISP == isp && (d.ActionTaken == "emergency_halt" || d.ActionTaken == "quarantine_ip") {
				warnings = append(warnings, "Recent "+d.ActionTaken+" decision detected")
			}
		}

		hourlyRate := dailyCap / 24
		if hourlyRate <= 0 {
			hourlyRate = 0
		}

		isps = append(isps, ISPReadiness{
			ISP:              isp,
			DisplayName:      ispName(isp),
			HealthScore:      health,
			Status:           status,
			ActiveAgents:     activeAgents,
			TotalAgents:      6,
			BounceRate:       snap.BounceRate1h,
			DeferralRate:     snap.DeferralRate5m,
			ComplaintRate:    snap.ComplaintRate1h,
			WarmupIPs:        warmupIPs,
			ActiveIPs:        activeIPs,
			QuarantinedIPs:   quarantinedIPs,
			MaxDailyCapacity: dailyCap,
			MaxHourlyRate:    hourlyRate,
			PoolName:         PoolNameForISP(isp),
			HasEmergency:     hasEmergency,
			Warnings:         warnings,
		})
		totalCap += dailyCap
	}

	overall := "ready"
	if overallBlocked {
		overall = "blocked"
	} else {
		for _, r := range isps {
			if r.Status == "caution" {
				overall = "caution"
				break
			}
		}
	}

	return CampaignReadinessResponse{
		ISPs:          isps,
		TotalCapacity: totalCap,
		OverallStatus: overall,
	}
}

func filterStates(states []AgentState, isp ISP) []AgentState {
	var out []AgentState
	for _, s := range states {
		if s.ISP == isp {
			out = append(out, s)
		}
	}
	return out
}

func ispName(isp ISP) string {
	names := map[ISP]string{
		ISPGmail: "Gmail", ISPYahoo: "Yahoo", ISPMicrosoft: "Microsoft",
		ISPApple: "Apple iCloud", ISPComcast: "Comcast", ISPAtt: "AT&T",
		ISPCox: "Cox", ISPCharter: "Charter/Spectrum",
	}
	if n, ok := names[isp]; ok {
		return n
	}
	return string(isp)
}

// GetDecisions returns decisions from the database for an ISP.
func (o *Orchestrator) GetDecisions(ctx context.Context, isp string, limit int) ([]Decision, error) {
	var ispPtr *ISP
	if isp != "" {
		i := ISP(isp)
		ispPtr = &i
	}
	return o.decisions.QueryDecisions(ctx, o.orgID, ispPtr, nil, nil, limit)
}
