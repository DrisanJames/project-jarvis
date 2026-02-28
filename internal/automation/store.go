package automation

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Store handles CRUD for automation_flows and automation_executions tables.
type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

func (s *Store) GetFlow(ctx context.Context, id uuid.UUID) (*Flow, error) {
	var f Flow
	var stepsJSON []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT id, organization_id, name, COALESCE(description,''), trigger_event, steps, status, created_at, updated_at
		FROM automation_flows WHERE id = $1`, id,
	).Scan(&f.ID, &f.OrganizationID, &f.Name, &f.Description, &f.TriggerEvent, &stepsJSON, &f.Status, &f.CreatedAt, &f.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	json.Unmarshal(stepsJSON, &f.Steps)
	return &f, nil
}

func (s *Store) ListFlowsByTrigger(ctx context.Context, triggerEvent string) ([]Flow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, organization_id, name, COALESCE(description,''), trigger_event, steps, status, created_at, updated_at
		FROM automation_flows WHERE trigger_event = $1 AND status = 'active'`, triggerEvent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var flows []Flow
	for rows.Next() {
		var f Flow
		var stepsJSON []byte
		if err := rows.Scan(&f.ID, &f.OrganizationID, &f.Name, &f.Description, &f.TriggerEvent, &stepsJSON, &f.Status, &f.CreatedAt, &f.UpdatedAt); err != nil {
			continue
		}
		json.Unmarshal(stepsJSON, &f.Steps)
		flows = append(flows, f)
	}
	return flows, rows.Err()
}

func (s *Store) CreateFlow(ctx context.Context, f *Flow) error {
	if f.ID == uuid.Nil {
		f.ID = uuid.New()
	}
	stepsJSON, _ := json.Marshal(f.Steps)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO automation_flows (id, organization_id, name, description, trigger_event, steps, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		f.ID, f.OrganizationID, f.Name, f.Description, f.TriggerEvent, stepsJSON, f.Status)
	return err
}

func (s *Store) UpdateFlow(ctx context.Context, f *Flow) error {
	stepsJSON, _ := json.Marshal(f.Steps)
	_, err := s.db.ExecContext(ctx,
		`UPDATE automation_flows SET name=$1, description=$2, steps=$3, status=$4, updated_at=NOW()
		WHERE id = $5`,
		f.Name, f.Description, stepsJSON, f.Status, f.ID)
	return err
}

func (s *Store) CreateExecution(ctx context.Context, e *Execution) error {
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO automation_executions (id, flow_id, subscriber_id, email, current_step, status, next_run_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		e.ID, e.FlowID, e.SubscriberID, e.Email, e.CurrentStep, e.Status, e.NextRunAt)
	return err
}

func (s *Store) GetExecution(ctx context.Context, id uuid.UUID) (*Execution, error) {
	var e Execution
	err := s.db.QueryRowContext(ctx,
		`SELECT id, flow_id, subscriber_id, email, current_step, status, next_run_at, completed_at, created_at, updated_at
		FROM automation_executions WHERE id = $1`, id,
	).Scan(&e.ID, &e.FlowID, &e.SubscriberID, &e.Email, &e.CurrentStep, &e.Status, &e.NextRunAt, &e.CompletedAt, &e.CreatedAt, &e.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func (s *Store) UpdateExecution(ctx context.Context, e *Execution) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE automation_executions SET current_step=$1, status=$2, next_run_at=$3, completed_at=$4, updated_at=NOW()
		WHERE id = $5`,
		e.CurrentStep, e.Status, e.NextRunAt, e.CompletedAt, e.ID)
	return err
}

func (s *Store) ListPendingExecutions(ctx context.Context, before time.Time, limit int) ([]Execution, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, flow_id, subscriber_id, email, current_step, status, next_run_at, completed_at, created_at, updated_at
		FROM automation_executions WHERE status = 'running' AND next_run_at <= $1 LIMIT $2`,
		before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var execs []Execution
	for rows.Next() {
		var e Execution
		if err := rows.Scan(&e.ID, &e.FlowID, &e.SubscriberID, &e.Email, &e.CurrentStep, &e.Status, &e.NextRunAt, &e.CompletedAt, &e.CreatedAt, &e.UpdatedAt); err != nil {
			continue
		}
		execs = append(execs, e)
	}
	return execs, rows.Err()
}

// ExistsExecution checks if an execution already exists for a subscriber+flow (H17).
func (s *Store) ExistsExecution(ctx context.Context, subscriberID, flowID uuid.UUID) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM automation_executions
		WHERE subscriber_id = $1 AND flow_id = $2 AND status IN ('running', 'completed')`,
		subscriberID, flowID).Scan(&count)
	return count > 0, err
}
