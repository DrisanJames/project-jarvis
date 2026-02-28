package automation

import (
	"context"
	"database/sql"
	"log"
	"time"

	"github.com/google/uuid"
)

// FlowEngine listens for trigger events and executes automation flows.
type FlowEngine struct {
	db        *sql.DB
	store     *Store
	sender    EmailSender
	interval  time.Duration
	ctx       context.Context
	cancel    context.CancelFunc
	lastRunAt time.Time
	healthy   bool
}

func NewFlowEngine(db *sql.DB, sender EmailSender) *FlowEngine {
	return &FlowEngine{
		db:       db,
		store:    NewStore(db),
		sender:   sender,
		interval: 30 * time.Second,
		healthy:  true,
	}
}

func (fe *FlowEngine) Start() {
	fe.ctx, fe.cancel = context.WithCancel(context.Background())
	go func() {
		log.Println("[FlowEngine] Starting automation flow engine")
		time.Sleep(15 * time.Second)

		ticker := time.NewTicker(fe.interval)
		defer ticker.Stop()
		for {
			select {
			case <-fe.ctx.Done():
				log.Println("[FlowEngine] Stopped")
				return
			case <-ticker.C:
				fe.processPending()
			}
		}
	}()
}

func (fe *FlowEngine) Stop() {
	if fe.cancel != nil {
		fe.cancel()
	}
}

func (fe *FlowEngine) IsHealthy() bool  { return fe.healthy }
func (fe *FlowEngine) LastRunAt() time.Time { return fe.lastRunAt }

// Trigger starts automation flows matching the given event for a subscriber.
// It checks for existing executions to prevent duplicates (H17).
func (fe *FlowEngine) Trigger(ctx context.Context, event string, subscriberID uuid.UUID, email string) error {
	flows, err := fe.store.ListFlowsByTrigger(ctx, event)
	if err != nil {
		return err
	}

	for _, flow := range flows {
		exists, err := fe.store.ExistsExecution(ctx, subscriberID, flow.ID)
		if err != nil {
			log.Printf("[FlowEngine] check existing exec error: %v", err)
			continue
		}
		if exists {
			log.Printf("[FlowEngine] skipping duplicate trigger for subscriber=%s flow=%s", subscriberID, flow.ID)
			continue
		}

		now := time.Now()
		exec := &Execution{
			FlowID:       flow.ID,
			SubscriberID: subscriberID,
			Email:        email,
			CurrentStep:  0,
			Status:       "running",
			NextRunAt:    &now,
		}

		if err := fe.store.CreateExecution(ctx, exec); err != nil {
			log.Printf("[FlowEngine] create execution error: %v", err)
			continue
		}

		// Execute step 0 immediately if delay is 0
		if len(flow.Steps) > 0 && flow.Steps[0].DelayHours == 0 {
			fe.advanceExecution(ctx, exec, &flow)
		}
	}
	return nil
}

func (fe *FlowEngine) processPending() {
	fe.lastRunAt = time.Now()
	fe.healthy = true
	ctx := fe.ctx

	execs, err := fe.store.ListPendingExecutions(ctx, time.Now(), 100)
	if err != nil {
		log.Printf("[FlowEngine] list pending error: %v", err)
		fe.healthy = false
		return
	}

	for _, exec := range execs {
		if ctx.Err() != nil {
			return
		}
		flow, err := fe.store.GetFlow(ctx, exec.FlowID)
		if err != nil || flow == nil {
			exec.Status = "failed"
			fe.store.UpdateExecution(ctx, &exec)
			continue
		}
		fe.advanceExecution(ctx, &exec, flow)
	}
}

func (fe *FlowEngine) advanceExecution(ctx context.Context, exec *Execution, flow *Flow) {
	if exec.CurrentStep >= len(flow.Steps) {
		now := time.Now()
		exec.Status = "completed"
		exec.CompletedAt = &now
		fe.store.UpdateExecution(ctx, exec)
		return
	}

	step := flow.Steps[exec.CurrentStep]

	switch step.Type {
	case "send_email":
		if fe.sender != nil {
			subject, html := fe.loadTemplate(ctx, step.Template, flow.OrganizationID)
			if err := fe.sender.SendTransactional(ctx, flow.OrganizationID.String(), exec.Email, subject, html); err != nil {
				log.Printf("[FlowEngine] send error exec=%s step=%d: %v", exec.ID, exec.CurrentStep, err)
			}
		}
		exec.CurrentStep++
		fe.setNextRun(exec, flow)

	case "wait":
		exec.CurrentStep++
		if exec.CurrentStep < len(flow.Steps) {
			next := time.Now().Add(time.Duration(step.DelayHours) * time.Hour)
			exec.NextRunAt = &next
		}

	case "condition":
		passed := fe.evaluateCondition(ctx, step.Check, exec.SubscriberID)
		if !passed && step.OnFalse == "skip_to_end" {
			now := time.Now()
			exec.Status = "completed"
			exec.CompletedAt = &now
		} else {
			exec.CurrentStep++
			fe.setNextRun(exec, flow)
		}
	}

	fe.store.UpdateExecution(ctx, exec)
}

func (fe *FlowEngine) setNextRun(exec *Execution, flow *Flow) {
	if exec.CurrentStep >= len(flow.Steps) {
		now := time.Now()
		exec.Status = "completed"
		exec.CompletedAt = &now
		return
	}

	nextStep := flow.Steps[exec.CurrentStep]
	if nextStep.DelayHours > 0 {
		next := time.Now().Add(time.Duration(nextStep.DelayHours) * time.Hour)
		exec.NextRunAt = &next
	} else {
		now := time.Now()
		exec.NextRunAt = &now
	}
}

func (fe *FlowEngine) loadTemplate(ctx context.Context, templateName string, orgID uuid.UUID) (string, string) {
	var subject, html string
	err := fe.db.QueryRowContext(ctx,
		`SELECT subject, html_content FROM mailing_templates
		WHERE organization_id = $1 AND name = $2 AND status = 'active'
		LIMIT 1`, orgID, templateName,
	).Scan(&subject, &html)
	if err != nil {
		return templateName, "<html><body><p>Template not found: " + templateName + "</p></body></html>"
	}
	return subject, html
}

func (fe *FlowEngine) evaluateCondition(ctx context.Context, check string, subscriberID uuid.UUID) bool {
	switch check {
	case "email_verified":
		var score float64
		fe.db.QueryRowContext(ctx,
			`SELECT COALESCE(data_quality_score, 0) FROM mailing_subscribers WHERE id = $1`,
			subscriberID).Scan(&score)
		return score >= 0.50
	case "has_opened":
		var count int
		fe.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM mailing_tracking_events WHERE subscriber_id = $1 AND event_type = 'opened'`,
			subscriberID).Scan(&count)
		return count > 0
	default:
		return true
	}
}
