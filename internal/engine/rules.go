package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
)

// RuleStore manages governance rules in the database.
type RuleStore struct {
	db    *sql.DB
	orgID string
}

// NewRuleStore creates a new rule store.
func NewRuleStore(db *sql.DB, orgID string) *RuleStore {
	return &RuleStore{db: db, orgID: orgID}
}

// ListRules returns all rules, optionally filtered by ISP and agent type.
func (rs *RuleStore) ListRules(ctx context.Context, isp string, agentType string) ([]Rule, error) {
	query := `SELECT id, organization_id, isp, agent_type, name, COALESCE(description,''),
		metric, operator, threshold, window_seconds, action, action_params,
		cooldown_seconds, priority, enabled, created_at, updated_at
		FROM mailing_engine_rules WHERE organization_id = $1`
	args := []interface{}{rs.orgID}
	argN := 2

	if isp != "" {
		query += fmt.Sprintf(" AND isp = $%d", argN)
		args = append(args, isp)
		argN++
	}
	if agentType != "" {
		query += fmt.Sprintf(" AND agent_type = $%d", argN)
		args = append(args, agentType)
		argN++
	}
	query += " ORDER BY priority DESC, created_at"

	rows, err := rs.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rules []Rule
	for rows.Next() {
		var r Rule
		if err := rows.Scan(&r.ID, &r.OrganizationID, &r.ISP, &r.AgentType,
			&r.Name, &r.Description, &r.Metric, &r.Operator, &r.Threshold,
			&r.WindowSeconds, &r.Action, &r.ActionParams, &r.CooldownSeconds,
			&r.Priority, &r.Enabled, &r.CreatedAt, &r.UpdatedAt); err != nil {
			continue
		}
		rules = append(rules, r)
	}
	return rules, rows.Err()
}

// CreateRule inserts a new rule.
func (rs *RuleStore) CreateRule(ctx context.Context, r Rule) (*Rule, error) {
	r.OrganizationID = rs.orgID
	if r.ActionParams == nil {
		r.ActionParams = json.RawMessage("{}")
	}
	err := rs.db.QueryRowContext(ctx,
		`INSERT INTO mailing_engine_rules
		(organization_id, isp, agent_type, name, description, metric, operator, threshold,
		 window_seconds, action, action_params, cooldown_seconds, priority, enabled)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		RETURNING id, created_at, updated_at`,
		r.OrganizationID, r.ISP, r.AgentType, r.Name, r.Description,
		r.Metric, r.Operator, r.Threshold, r.WindowSeconds,
		r.Action, r.ActionParams, r.CooldownSeconds, r.Priority, r.Enabled,
	).Scan(&r.ID, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// UpdateRule modifies an existing rule.
func (rs *RuleStore) UpdateRule(ctx context.Context, id string, r Rule) (*Rule, error) {
	if r.ActionParams == nil {
		r.ActionParams = json.RawMessage("{}")
	}
	err := rs.db.QueryRowContext(ctx,
		`UPDATE mailing_engine_rules SET
		isp=$1, agent_type=$2, name=$3, description=$4, metric=$5, operator=$6,
		threshold=$7, window_seconds=$8, action=$9, action_params=$10,
		cooldown_seconds=$11, priority=$12, enabled=$13, updated_at=NOW()
		WHERE id=$14 AND organization_id=$15
		RETURNING id, created_at, updated_at`,
		r.ISP, r.AgentType, r.Name, r.Description, r.Metric, r.Operator,
		r.Threshold, r.WindowSeconds, r.Action, r.ActionParams,
		r.CooldownSeconds, r.Priority, r.Enabled, id, rs.orgID,
	).Scan(&r.ID, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, err
	}
	r.OrganizationID = rs.orgID
	return &r, nil
}

// DeleteRule removes a rule by ID.
func (rs *RuleStore) DeleteRule(ctx context.Context, id string) error {
	_, err := rs.db.ExecContext(ctx,
		`DELETE FROM mailing_engine_rules WHERE id = $1 AND organization_id = $2`,
		id, rs.orgID)
	return err
}

// SeedDefaultRules inserts ISP-specific default rules if none exist.
func (rs *RuleStore) SeedDefaultRules(ctx context.Context, configs []ISPConfig) {
	var count int
	rs.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mailing_engine_rules WHERE organization_id = $1`, rs.orgID,
	).Scan(&count)
	if count > 0 {
		return
	}

	log.Printf("[rules] seeding default rules for %d ISPs", len(configs))

	for _, cfg := range configs {
		defaults := []Rule{
			{ISP: cfg.ISP, AgentType: AgentReputation, Name: "Bounce Warning",
				Metric: "bounce_rate_1h", Operator: ">", Threshold: cfg.BounceWarnPct,
				WindowSeconds: 3600, Action: "warn_bounce_rate", CooldownSeconds: 300, Priority: 40, Enabled: true},
			{ISP: cfg.ISP, AgentType: AgentReputation, Name: "Bounce Action",
				Metric: "bounce_rate_1h", Operator: ">", Threshold: cfg.BounceActionPct,
				WindowSeconds: 3600, Action: "disable_source_ip", CooldownSeconds: 600, Priority: 80, Enabled: true},
			{ISP: cfg.ISP, AgentType: AgentReputation, Name: "Complaint Warning",
				Metric: "complaint_rate_24h", Operator: ">", Threshold: cfg.ComplaintWarnPct,
				WindowSeconds: 86400, Action: "warn_complaint_rate", CooldownSeconds: 300, Priority: 40, Enabled: true},
			{ISP: cfg.ISP, AgentType: AgentReputation, Name: "Complaint Action",
				Metric: "complaint_rate_24h", Operator: ">", Threshold: cfg.ComplaintActionPct,
				WindowSeconds: 86400, Action: "quarantine_ip", CooldownSeconds: 600, Priority: 80, Enabled: true},
			{ISP: cfg.ISP, AgentType: AgentThrottle, Name: "Deferral Throttle",
				Metric: "deferral_rate_5m", Operator: ">", Threshold: 20,
				WindowSeconds: 300, Action: "reduce_rate", CooldownSeconds: 300, Priority: 60, Enabled: true},
			{ISP: cfg.ISP, AgentType: AgentEmergency, Name: "Bounce Spike",
				Metric: "bounce_rate_5m", Operator: ">", Threshold: 25,
				WindowSeconds: 300, Action: "emergency_halt", CooldownSeconds: 0, Priority: 100, Enabled: true},
			{ISP: cfg.ISP, AgentType: AgentEmergency, Name: "Deferral Spike",
				Metric: "deferral_rate_5m", Operator: ">", Threshold: 50,
				WindowSeconds: 300, Action: "emergency_halt", CooldownSeconds: 0, Priority: 100, Enabled: true},
		}
		for _, r := range defaults {
			rs.CreateRule(ctx, r)
		}
	}
}
