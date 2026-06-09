package domainagent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Sentinel errors so HTTP handlers can map storage outcomes to 404/409.
var (
	ErrPlanNotFound    = errors.New("plan not found")
	ErrPlanNotEditable = errors.New("plan is not editable in its current status")
)

// Plan is one row of mailing_domain_agent_plans. Briefing/Slots/Payloads/
// DeployResults are kept as raw JSON so operator-authored payloads (including
// extra keys like "_audit") round-trip byte-faithful through storage.
type Plan struct {
	ID             string
	OrganizationID string
	SendingDomain  string
	PlanDate       time.Time
	Status         string
	Briefing       json.RawMessage
	Slots          json.RawMessage
	Payloads       json.RawMessage
	DeployResults  json.RawMessage
	ApprovedBy     string
	ApprovedAt     *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// PlanStore persists Domain Agent plans.
type PlanStore struct {
	db *sql.DB
}

func NewPlanStore(db *sql.DB) *PlanStore {
	return &PlanStore{db: db}
}

const planColumns = `id, organization_id, sending_domain, plan_date, status,
	briefing::text, slots::text, payloads::text, deploy_results::text,
	COALESCE(approved_by, ''), approved_at, created_at, updated_at`

type rowScanner interface {
	Scan(dest ...interface{}) error
}

func scanPlan(row rowScanner) (*Plan, error) {
	var p Plan
	var briefing, slots, payloads, deployResults string
	var approvedAt sql.NullTime
	err := row.Scan(&p.ID, &p.OrganizationID, &p.SendingDomain, &p.PlanDate, &p.Status,
		&briefing, &slots, &payloads, &deployResults,
		&p.ApprovedBy, &approvedAt, &p.CreatedAt, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrPlanNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan plan: %w", err)
	}
	p.Briefing = json.RawMessage(briefing)
	p.Slots = json.RawMessage(slots)
	p.Payloads = json.RawMessage(payloads)
	p.DeployResults = json.RawMessage(deployResults)
	if approvedAt.Valid {
		t := approvedAt.Time
		p.ApprovedAt = &t
	}
	return &p, nil
}

// Get fetches a plan by id, scoped to the org.
func (s *PlanStore) Get(ctx context.Context, orgID, id string) (*Plan, error) {
	return scanPlan(s.db.QueryRowContext(ctx, `
		SELECT `+planColumns+`
		FROM mailing_domain_agent_plans
		WHERE organization_id = $1 AND id = $2
	`, orgID, id))
}

// GetByDomainDate fetches the plan for (org, domain, date) if one exists.
func (s *PlanStore) GetByDomainDate(ctx context.Context, orgID, domain string, date time.Time) (*Plan, error) {
	return scanPlan(s.db.QueryRowContext(ctx, `
		SELECT `+planColumns+`
		FROM mailing_domain_agent_plans
		WHERE organization_id = $1 AND sending_domain = $2 AND plan_date = $3
	`, orgID, domain, utcDate(date).Format("2006-01-02")))
}

// CreateDraft inserts a new draft plan carrying the generated briefing.
func (s *PlanStore) CreateDraft(ctx context.Context, orgID, domain string, date time.Time, briefing json.RawMessage) (*Plan, error) {
	if len(briefing) == 0 {
		briefing = json.RawMessage(`{}`)
	}
	return scanPlan(s.db.QueryRowContext(ctx, `
		INSERT INTO mailing_domain_agent_plans (organization_id, sending_domain, plan_date, status, briefing)
		VALUES ($1, $2, $3, 'draft', $4::jsonb)
		RETURNING `+planColumns, orgID, domain, utcDate(date).Format("2006-01-02"), string(briefing)))
}

// guardedUpdate runs an UPDATE that includes its own status guard and maps a
// zero-row result to ErrPlanNotFound / ErrPlanNotEditable.
func (s *PlanStore) guardedUpdate(ctx context.Context, orgID, id, query string, args ...interface{}) error {
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("update plan: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	var status string
	err = s.db.QueryRowContext(ctx, `
		SELECT status FROM mailing_domain_agent_plans WHERE organization_id = $1 AND id = $2
	`, orgID, id).Scan(&status)
	if err == sql.ErrNoRows {
		return ErrPlanNotFound
	}
	if err != nil {
		return fmt.Errorf("check plan status: %w", err)
	}
	return fmt.Errorf("%w (status=%s)", ErrPlanNotEditable, status)
}

// UpdateSlots replaces the slots array. Only draft/compiled plans are editable.
func (s *PlanStore) UpdateSlots(ctx context.Context, orgID, id string, slots json.RawMessage) error {
	return s.guardedUpdate(ctx, orgID, id, `
		UPDATE mailing_domain_agent_plans
		SET slots = $3::jsonb, updated_at = NOW()
		WHERE organization_id = $1 AND id = $2 AND status IN ('draft','compiled')
	`, orgID, id, string(slots))
}

// UpdateBriefing replaces the briefing. Only draft/compiled plans are editable.
func (s *PlanStore) UpdateBriefing(ctx context.Context, orgID, id string, briefing json.RawMessage) error {
	return s.guardedUpdate(ctx, orgID, id, `
		UPDATE mailing_domain_agent_plans
		SET briefing = $3::jsonb, updated_at = NOW()
		WHERE organization_id = $1 AND id = $2 AND status IN ('draft','compiled')
	`, orgID, id, string(briefing))
}

// AttachPayloads stores the compiled deploy payloads and advances the plan to
// 'compiled'. Re-compiling an already-compiled plan is allowed.
func (s *PlanStore) AttachPayloads(ctx context.Context, orgID, id string, payloads json.RawMessage) error {
	return s.guardedUpdate(ctx, orgID, id, `
		UPDATE mailing_domain_agent_plans
		SET payloads = $3::jsonb, status = 'compiled', updated_at = NOW()
		WHERE organization_id = $1 AND id = $2 AND status IN ('draft','compiled')
	`, orgID, id, string(payloads))
}

// Approve transitions compiled → approved and records the approver.
func (s *PlanStore) Approve(ctx context.Context, orgID, id, approvedBy string) error {
	return s.guardedUpdate(ctx, orgID, id, `
		UPDATE mailing_domain_agent_plans
		SET status = 'approved', approved_by = $3, approved_at = NOW(), updated_at = NOW()
		WHERE organization_id = $1 AND id = $2 AND status = 'compiled'
	`, orgID, id, approvedBy)
}

// SetDeployResults records the per-payload deploy outcomes and the final
// plan status ('deployed' or 'failed').
func (s *PlanStore) SetDeployResults(ctx context.Context, orgID, id string, results json.RawMessage, finalStatus string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE mailing_domain_agent_plans
		SET deploy_results = $3::jsonb, status = $4, updated_at = NOW()
		WHERE organization_id = $1 AND id = $2
	`, orgID, id, string(results), finalStatus)
	if err != nil {
		return fmt.Errorf("set deploy results: %w", err)
	}
	return nil
}
