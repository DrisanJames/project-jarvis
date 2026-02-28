package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"time"
)

// DBDecisionStore implements DecisionStore using *sql.DB.
type DBDecisionStore struct {
	DB *sql.DB
}

func (ds *DBDecisionStore) PersistDecision(ctx context.Context, d Decision) error {
	if d.SignalValues == nil {
		d.SignalValues = json.RawMessage("{}")
	}
	if d.ActionParams == nil {
		d.ActionParams = json.RawMessage("{}")
	}
	_, err := ds.DB.ExecContext(ctx,
		`INSERT INTO mailing_engine_decisions
		(organization_id, isp, agent_type, signal_values, action_taken, action_params,
		 target_type, target_value, result)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		d.OrganizationID, d.ISP, d.AgentType, d.SignalValues,
		d.ActionTaken, d.ActionParams, d.TargetType, d.TargetValue, d.Result,
	)
	return err
}

func (ds *DBDecisionStore) PersistAgentState(ctx context.Context, orgID string, isp ISP, agentType AgentType, status AgentStatus) error {
	_, err := ds.DB.ExecContext(ctx,
		`INSERT INTO mailing_engine_agent_state
		(organization_id, isp, agent_type, status, last_eval_at, decisions_count, current_actions)
		VALUES ($1,$2,$3,$4,NOW(),1,'[]')
		ON CONFLICT (organization_id, isp, agent_type) DO UPDATE SET
		status = $4, last_eval_at = NOW(), decisions_count = mailing_engine_agent_state.decisions_count + 1,
		updated_at = NOW()`,
		orgID, isp, agentType, status,
	)
	return err
}

func (ds *DBDecisionStore) GetAgentStates(ctx context.Context, orgID string) ([]AgentState, error) {
	rows, err := ds.DB.QueryContext(ctx,
		`SELECT id, organization_id, isp, agent_type, status, last_eval_at,
		 decisions_count, current_actions, COALESCE(error_message,''),
		 COALESCE(s3_state_key,''), created_at, updated_at
		 FROM mailing_engine_agent_state WHERE organization_id = $1`,
		orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var states []AgentState
	for rows.Next() {
		var s AgentState
		if err := rows.Scan(&s.ID, &s.OrganizationID, &s.ISP, &s.AgentType,
			&s.Status, &s.LastEvalAt, &s.DecisionsCount, &s.CurrentActions,
			&s.ErrorMessage, &s.S3StateKey, &s.CreatedAt, &s.UpdatedAt); err != nil {
			continue
		}
		states = append(states, s)
	}
	return states, rows.Err()
}

func (ds *DBDecisionStore) GetISPAgentStates(ctx context.Context, orgID string, isp ISP) ([]AgentState, error) {
	rows, err := ds.DB.QueryContext(ctx,
		`SELECT id, organization_id, isp, agent_type, status, last_eval_at,
		 decisions_count, current_actions, COALESCE(error_message,''),
		 COALESCE(s3_state_key,''), created_at, updated_at
		 FROM mailing_engine_agent_state WHERE organization_id = $1 AND isp = $2`,
		orgID, isp)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var states []AgentState
	for rows.Next() {
		var s AgentState
		if err := rows.Scan(&s.ID, &s.OrganizationID, &s.ISP, &s.AgentType,
			&s.Status, &s.LastEvalAt, &s.DecisionsCount, &s.CurrentActions,
			&s.ErrorMessage, &s.S3StateKey, &s.CreatedAt, &s.UpdatedAt); err != nil {
			continue
		}
		states = append(states, s)
	}
	return states, rows.Err()
}

func (ds *DBDecisionStore) UpdateAgentStatus(ctx context.Context, orgID string, isp ISP, agentType AgentType, status AgentStatus) error {
	_, err := ds.DB.ExecContext(ctx,
		`UPDATE mailing_engine_agent_state SET status = $4, updated_at = NOW()
		 WHERE organization_id = $1 AND isp = $2 AND agent_type = $3`,
		orgID, isp, agentType, status)
	return err
}

func (ds *DBDecisionStore) QueryDecisions(ctx context.Context, orgID string, isp *ISP, agentType *AgentType, since *time.Time, limit int) ([]Decision, error) {
	var query string
	var args []interface{}

	if isp != nil {
		query = `SELECT id, organization_id, isp, agent_type, signal_values,
			action_taken, action_params, COALESCE(target_type,''), COALESCE(target_value,''),
			result, reverted_at, COALESCE(revert_reason,''), created_at
			FROM mailing_engine_decisions WHERE organization_id = $1 AND isp = $2
			ORDER BY created_at DESC LIMIT $3`
		args = []interface{}{orgID, *isp, limit}
	} else {
		query = `SELECT id, organization_id, isp, agent_type, signal_values,
			action_taken, action_params, COALESCE(target_type,''), COALESCE(target_value,''),
			result, reverted_at, COALESCE(revert_reason,''), created_at
			FROM mailing_engine_decisions WHERE organization_id = $1
			ORDER BY created_at DESC LIMIT $2`
		args = []interface{}{orgID, limit}
	}

	rows, err := ds.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var decisions []Decision
	for rows.Next() {
		var d Decision
		if err := rows.Scan(&d.ID, &d.OrganizationID, &d.ISP, &d.AgentType,
			&d.SignalValues, &d.ActionTaken, &d.ActionParams,
			&d.TargetType, &d.TargetValue, &d.Result,
			&d.RevertedAt, &d.RevertReason, &d.CreatedAt); err != nil {
			continue
		}
		decisions = append(decisions, d)
	}
	return decisions, rows.Err()
}

func (ds *DBDecisionStore) QueryIPWarmupState(ctx context.Context, orgID string, poolName string) (activeIPs, warmupIPs, quarantinedIPs, dailyCap int, err error) {
	rows, err := ds.DB.QueryContext(ctx,
		`SELECT status, warmup_daily_limit FROM mailing_ip_addresses
		 WHERE organization_id = $1 AND pool_id IN (
		   SELECT id FROM mailing_ip_pools WHERE name = $2
		 )`, orgID, poolName)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	defer rows.Close()

	for rows.Next() {
		var st string
		var limit int
		if err := rows.Scan(&st, &limit); err != nil {
			continue
		}
		switch st {
		case "active":
			activeIPs++
			dailyCap += 50000
		case "warmup":
			warmupIPs++
			dailyCap += limit
		case "quarantined":
			quarantinedIPs++
		}
	}
	return activeIPs, warmupIPs, quarantinedIPs, dailyCap, rows.Err()
}

// DBSignalStore implements SignalStore using *sql.DB.
type DBSignalStore struct {
	DB *sql.DB
}

func (ss *DBSignalStore) PersistSignals(ctx context.Context, orgID string, snap SignalSnapshot, metrics []SignalMetric) error {
	for _, m := range metrics {
		_, err := ss.DB.ExecContext(ctx,
			`INSERT INTO mailing_engine_signals
			(organization_id, isp, metric_name, dimension_type, dimension_value, value, window_seconds, sample_count)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 1)`,
			orgID, snap.ISP, m.MetricName, m.DimensionType, m.DimensionValue, m.Value, m.WindowSeconds,
		)
		if err != nil {
			log.Printf("[signals] persist error isp=%s metric=%s: %v", snap.ISP, m.MetricName, err)
		}
	}
	return nil
}

// DBSuppressionRepo implements SuppressionRepository using *sql.DB.
type DBSuppressionRepo struct {
	DB *sql.DB
}

func (r *DBSuppressionRepo) LoadAll(ctx context.Context, orgID string) (map[ISP][]string, error) {
	rows, err := r.DB.QueryContext(ctx,
		`SELECT email, isp FROM mailing_engine_suppressions WHERE organization_id = $1`,
		orgID)
	if err != nil {
		return nil, fmt.Errorf("load suppressions: %w", err)
	}
	defer rows.Close()

	result := make(map[ISP][]string)
	for rows.Next() {
		var email string
		var isp ISP
		if err := rows.Scan(&email, &isp); err != nil {
			continue
		}
		result[isp] = append(result[isp], email)
	}
	return result, rows.Err()
}

func (r *DBSuppressionRepo) Add(ctx context.Context, s Suppression) error {
	_, err := r.DB.ExecContext(ctx,
		`INSERT INTO mailing_engine_suppressions
		(organization_id, email, isp, reason, dsn_code, dsn_diagnostic, source_ip, source_vmta, campaign_id, suppressed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (organization_id, email, isp) DO NOTHING`,
		s.OrganizationID, s.Email, s.ISP, s.Reason,
		s.DSNCode, s.DSNDiagnostic, s.SourceIP, s.SourceVMTA, s.CampaignID, s.SuppressedAt,
	)
	return err
}

func (r *DBSuppressionRepo) Remove(ctx context.Context, orgID string, isp ISP, email string) error {
	_, err := r.DB.ExecContext(ctx,
		`DELETE FROM mailing_engine_suppressions WHERE organization_id = $1 AND email = $2 AND isp = $3`,
		orgID, email, isp)
	return err
}

func (r *DBSuppressionRepo) Get(ctx context.Context, orgID string, isp ISP, email string) (*Suppression, error) {
	var s Suppression
	err := r.DB.QueryRowContext(ctx,
		`SELECT id, organization_id, email, isp, reason,
		 COALESCE(dsn_code,''), COALESCE(dsn_diagnostic,''),
		 COALESCE(source_ip::text,''), COALESCE(source_vmta,''),
		 COALESCE(campaign_id,''), suppressed_at, created_at
		 FROM mailing_engine_suppressions
		 WHERE organization_id = $1 AND email = $2 AND isp = $3`,
		orgID, email, isp,
	).Scan(&s.ID, &s.OrganizationID, &s.Email, &s.ISP, &s.Reason,
		&s.DSNCode, &s.DSNDiagnostic, &s.SourceIP, &s.SourceVMTA,
		&s.CampaignID, &s.SuppressedAt, &s.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (r *DBSuppressionRepo) List(ctx context.Context, orgID string, isp ISP, reason string, limit, offset int) ([]Suppression, int, error) {
	baseWhere := ` WHERE organization_id = $1 AND isp = $2`
	args := []interface{}{orgID, isp}
	argIdx := 3

	if reason != "" {
		baseWhere += fmt.Sprintf(` AND reason = $%d`, argIdx)
		args = append(args, reason)
		argIdx++
	}

	countQuery := `SELECT COUNT(*) FROM mailing_engine_suppressions` + baseWhere
	var total int
	if err := r.DB.QueryRowContext(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	listQuery := `SELECT id, organization_id, email, isp, reason,
		COALESCE(dsn_code,''), COALESCE(dsn_diagnostic,''),
		COALESCE(source_ip::text,''), COALESCE(source_vmta,''),
		COALESCE(campaign_id,''), suppressed_at, created_at
		FROM mailing_engine_suppressions` + baseWhere +
		fmt.Sprintf(` ORDER BY suppressed_at DESC LIMIT $%d OFFSET $%d`, argIdx, argIdx+1)
	args = append(args, limit, offset)

	rows, err := r.DB.QueryContext(ctx, listQuery, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var suppressions []Suppression
	for rows.Next() {
		var s Suppression
		if err := rows.Scan(&s.ID, &s.OrganizationID, &s.Email, &s.ISP, &s.Reason,
			&s.DSNCode, &s.DSNDiagnostic, &s.SourceIP, &s.SourceVMTA,
			&s.CampaignID, &s.SuppressedAt, &s.CreatedAt); err != nil {
			continue
		}
		suppressions = append(suppressions, s)
	}
	return suppressions, total, rows.Err()
}

func (r *DBSuppressionRepo) Stats(ctx context.Context, orgID string, isp ISP) (*SuppressionStats, error) {
	stats := &SuppressionStats{ISP: isp}

	r.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mailing_engine_suppressions WHERE organization_id = $1 AND isp = $2`,
		orgID, isp).Scan(&stats.TotalCount)

	r.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mailing_engine_suppressions
		 WHERE organization_id = $1 AND isp = $2 AND suppressed_at >= CURRENT_DATE`,
		orgID, isp).Scan(&stats.TodayCount)

	r.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mailing_engine_suppressions
		 WHERE organization_id = $1 AND isp = $2 AND suppressed_at >= NOW() - INTERVAL '24 hours'`,
		orgID, isp).Scan(&stats.Last24hCount)

	r.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mailing_engine_suppressions
		 WHERE organization_id = $1 AND isp = $2 AND suppressed_at >= NOW() - INTERVAL '1 hour'`,
		orgID, isp).Scan(&stats.Last1hCount)

	if stats.Last1hCount > 0 {
		stats.VelocityPerMin = float64(stats.Last1hCount) / 60.0
	}

	reasonRows, err := r.DB.QueryContext(ctx,
		`SELECT reason, COUNT(*) as cnt FROM mailing_engine_suppressions
		 WHERE organization_id = $1 AND isp = $2
		 GROUP BY reason ORDER BY cnt DESC LIMIT 10`,
		orgID, isp)
	if err == nil {
		defer reasonRows.Close()
		for reasonRows.Next() {
			var rc ReasonCount
			reasonRows.Scan(&rc.Reason, &rc.Count)
			stats.TopReasons = append(stats.TopReasons, rc)
		}
	}

	campRows, err := r.DB.QueryContext(ctx,
		`SELECT COALESCE(campaign_id,'unknown'), COUNT(*) as cnt FROM mailing_engine_suppressions
		 WHERE organization_id = $1 AND isp = $2 AND campaign_id IS NOT NULL
		 GROUP BY campaign_id ORDER BY cnt DESC LIMIT 10`,
		orgID, isp)
	if err == nil {
		defer campRows.Close()
		for campRows.Next() {
			var cc CampaignCount
			campRows.Scan(&cc.CampaignID, &cc.Count)
			stats.TopCampaigns = append(stats.TopCampaigns, cc)
		}
	}

	return stats, nil
}

func (r *DBSuppressionRepo) ListEmails(ctx context.Context, orgID string, isp ISP) ([]string, error) {
	rows, err := r.DB.QueryContext(ctx,
		`SELECT email FROM mailing_engine_suppressions
		 WHERE organization_id = $1 AND isp = $2 ORDER BY email`,
		orgID, isp)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var emails []string
	for rows.Next() {
		var email string
		if err := rows.Scan(&email); err != nil {
			continue
		}
		emails = append(emails, email)
	}
	return emails, rows.Err()
}

// Compile-time interface satisfaction checks
var (
	_ DecisionStore         = (*DBDecisionStore)(nil)
	_ SignalStore           = (*DBSignalStore)(nil)
	_ SuppressionRepository = (*DBSuppressionRepo)(nil)
)

