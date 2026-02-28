package mailing

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// ObjectivesStore handles campaign objectives database operations
type ObjectivesStore struct {
	db *sql.DB
}

// NewObjectivesStore creates a new objectives store
func NewObjectivesStore(db *sql.DB) *ObjectivesStore {
	return &ObjectivesStore{db: db}
}

// CreateObjective creates a new campaign objective
func (s *ObjectivesStore) CreateObjective(ctx context.Context, obj *CampaignObjective) error {
	// Generate ID if not set
	if obj.ID == uuid.Nil {
		obj.ID = uuid.New()
	}

	// Set defaults
	if obj.RotationStrategy == "" {
		obj.RotationStrategy = "performance"
	}
	if obj.ThroughputSensitivity == "" {
		obj.ThroughputSensitivity = "medium"
	}
	if obj.PacingStrategy == "" {
		obj.PacingStrategy = "even"
	}
	if obj.MinThroughputRate == 0 {
		obj.MinThroughputRate = 1000
	}
	if obj.MaxThroughputRate == 0 {
		obj.MaxThroughputRate = 100000
	}

	approvedCreatives, _ := json.Marshal(obj.ApprovedCreatives)
	everflowOfferIDs, _ := json.Marshal(obj.EverflowOfferIDs)

	query := `
		INSERT INTO mailing_campaign_objectives (
			id, campaign_id, organization_id, purpose,
			activation_goal, target_engagement_rate, target_clean_rate,
			warmup_daily_increment, warmup_max_daily_volume,
			offer_model, ecpm_target, budget_limit, budget_spent,
			target_metric, target_value, target_achieved,
			everflow_offer_ids, everflow_sub_id_template, property_code,
			approved_creatives, rotation_strategy, current_creative_index,
			ai_optimization_enabled, ai_throughput_optimization,
			ai_creative_rotation, ai_budget_pacing, esp_signal_monitoring,
			pause_on_spam_signal, spam_signal_threshold, bounce_threshold,
			throughput_sensitivity, min_throughput_rate, max_throughput_rate,
			target_completion_hours, pacing_strategy,
			created_at, updated_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
			$15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26,
			$27, $28, $29, $30, $31, $32, $33, $34, $35, NOW(), NOW()
		)
	`

	_, err := s.db.ExecContext(ctx, query,
		obj.ID, obj.CampaignID, obj.OrganizationID, obj.Purpose,
		objNullString(obj.ActivationGoal), obj.TargetEngagementRate, obj.TargetCleanRate,
		obj.WarmupDailyIncrement, obj.WarmupMaxDailyVolume,
		objNullString(obj.OfferModel), obj.ECPMTarget, obj.BudgetLimit, obj.BudgetSpent,
		objNullString(obj.TargetMetric), obj.TargetValue, obj.TargetAchieved,
		everflowOfferIDs, objNullString(obj.EverflowSubIDTemplate), objNullString(obj.PropertyCode),
		approvedCreatives, obj.RotationStrategy, obj.CurrentCreativeIndex,
		obj.AIOptimizationEnabled, obj.AIThroughputOptimization,
		obj.AICreativeRotation, obj.AIBudgetPacing, obj.ESPSignalMonitoring,
		obj.PauseOnSpamSignal, obj.SpamSignalThreshold, obj.BounceThreshold,
		obj.ThroughputSensitivity, obj.MinThroughputRate, obj.MaxThroughputRate,
		objNullInt(obj.TargetCompletionHours), obj.PacingStrategy,
	)

	return err
}

// GetObjective gets a campaign objective by campaign ID
func (s *ObjectivesStore) GetObjective(ctx context.Context, campaignID uuid.UUID) (*CampaignObjective, error) {
	query := `
		SELECT 
			id, campaign_id, organization_id, purpose,
			activation_goal, target_engagement_rate, target_clean_rate,
			warmup_daily_increment, warmup_max_daily_volume,
			offer_model, ecpm_target, budget_limit, budget_spent,
			target_metric, target_value, target_achieved,
			everflow_offer_ids, everflow_sub_id_template, property_code,
			approved_creatives, rotation_strategy, current_creative_index,
			ai_optimization_enabled, ai_throughput_optimization,
			ai_creative_rotation, ai_budget_pacing, esp_signal_monitoring,
			pause_on_spam_signal, spam_signal_threshold, bounce_threshold,
			throughput_sensitivity, min_throughput_rate, max_throughput_rate,
			target_completion_hours, pacing_strategy,
			created_at, updated_at
		FROM mailing_campaign_objectives
		WHERE campaign_id = $1
	`

	var obj CampaignObjective
	var activationGoal, offerModel, targetMetric, subIDTemplate, propertyCode sql.NullString
	var targetCompletionHours sql.NullInt64
	var approvedCreativesJSON, everflowOfferIDsJSON []byte

	err := s.db.QueryRowContext(ctx, query, campaignID).Scan(
		&obj.ID, &obj.CampaignID, &obj.OrganizationID, &obj.Purpose,
		&activationGoal, &obj.TargetEngagementRate, &obj.TargetCleanRate,
		&obj.WarmupDailyIncrement, &obj.WarmupMaxDailyVolume,
		&offerModel, &obj.ECPMTarget, &obj.BudgetLimit, &obj.BudgetSpent,
		&targetMetric, &obj.TargetValue, &obj.TargetAchieved,
		&everflowOfferIDsJSON, &subIDTemplate, &propertyCode,
		&approvedCreativesJSON, &obj.RotationStrategy, &obj.CurrentCreativeIndex,
		&obj.AIOptimizationEnabled, &obj.AIThroughputOptimization,
		&obj.AICreativeRotation, &obj.AIBudgetPacing, &obj.ESPSignalMonitoring,
		&obj.PauseOnSpamSignal, &obj.SpamSignalThreshold, &obj.BounceThreshold,
		&obj.ThroughputSensitivity, &obj.MinThroughputRate, &obj.MaxThroughputRate,
		&targetCompletionHours, &obj.PacingStrategy,
		&obj.CreatedAt, &obj.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}

	obj.ActivationGoal = activationGoal.String
	obj.OfferModel = offerModel.String
	obj.TargetMetric = targetMetric.String
	obj.EverflowSubIDTemplate = subIDTemplate.String
	obj.PropertyCode = propertyCode.String
	if targetCompletionHours.Valid {
		obj.TargetCompletionHours = int(targetCompletionHours.Int64)
	}

	if len(approvedCreativesJSON) > 0 {
		obj.ApprovedCreatives = approvedCreativesJSON
	}
	if len(everflowOfferIDsJSON) > 0 {
		obj.EverflowOfferIDs = everflowOfferIDsJSON
	}

	return &obj, nil
}

// UpdateObjective updates a campaign objective
func (s *ObjectivesStore) UpdateObjective(ctx context.Context, obj *CampaignObjective) error {
	approvedCreatives, _ := json.Marshal(obj.ApprovedCreatives)
	everflowOfferIDs, _ := json.Marshal(obj.EverflowOfferIDs)

	query := `
		UPDATE mailing_campaign_objectives SET
			purpose = $2,
			activation_goal = $3, target_engagement_rate = $4, target_clean_rate = $5,
			warmup_daily_increment = $6, warmup_max_daily_volume = $7,
			offer_model = $8, ecpm_target = $9, budget_limit = $10, budget_spent = $11,
			target_metric = $12, target_value = $13, target_achieved = $14,
			everflow_offer_ids = $15, everflow_sub_id_template = $16, property_code = $17,
			approved_creatives = $18, rotation_strategy = $19, current_creative_index = $20,
			ai_optimization_enabled = $21, ai_throughput_optimization = $22,
			ai_creative_rotation = $23, ai_budget_pacing = $24, esp_signal_monitoring = $25,
			pause_on_spam_signal = $26, spam_signal_threshold = $27, bounce_threshold = $28,
			throughput_sensitivity = $29, min_throughput_rate = $30, max_throughput_rate = $31,
			target_completion_hours = $32, pacing_strategy = $33,
			updated_at = NOW()
		WHERE campaign_id = $1
	`

	_, err := s.db.ExecContext(ctx, query,
		obj.CampaignID, obj.Purpose,
		objNullString(obj.ActivationGoal), obj.TargetEngagementRate, obj.TargetCleanRate,
		obj.WarmupDailyIncrement, obj.WarmupMaxDailyVolume,
		objNullString(obj.OfferModel), obj.ECPMTarget, obj.BudgetLimit, obj.BudgetSpent,
		objNullString(obj.TargetMetric), obj.TargetValue, obj.TargetAchieved,
		everflowOfferIDs, objNullString(obj.EverflowSubIDTemplate), objNullString(obj.PropertyCode),
		approvedCreatives, obj.RotationStrategy, obj.CurrentCreativeIndex,
		obj.AIOptimizationEnabled, obj.AIThroughputOptimization,
		obj.AICreativeRotation, obj.AIBudgetPacing, obj.ESPSignalMonitoring,
		obj.PauseOnSpamSignal, obj.SpamSignalThreshold, obj.BounceThreshold,
		obj.ThroughputSensitivity, obj.MinThroughputRate, obj.MaxThroughputRate,
		objNullInt(obj.TargetCompletionHours), obj.PacingStrategy,
	)

	return err
}

// DeleteObjective deletes a campaign objective
func (s *ObjectivesStore) DeleteObjective(ctx context.Context, campaignID uuid.UUID) error {
	_, err := s.db.ExecContext(ctx,
		"DELETE FROM mailing_campaign_objectives WHERE campaign_id = $1",
		campaignID,
	)
	return err
}

// UpdateBudgetSpent updates the budget spent for an objective
func (s *ObjectivesStore) UpdateBudgetSpent(ctx context.Context, campaignID uuid.UUID, spent float64) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE mailing_campaign_objectives SET budget_spent = $2, updated_at = NOW() WHERE campaign_id = $1",
		campaignID, spent,
	)
	return err
}

// UpdateTargetAchieved updates the target achieved for an objective
func (s *ObjectivesStore) UpdateTargetAchieved(ctx context.Context, campaignID uuid.UUID, achieved int) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE mailing_campaign_objectives SET target_achieved = $2, updated_at = NOW() WHERE campaign_id = $1",
		campaignID, achieved,
	)
	return err
}

// IncrementCreativeIndex increments the creative rotation index
func (s *ObjectivesStore) IncrementCreativeIndex(ctx context.Context, campaignID uuid.UUID) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE mailing_campaign_objectives 
		SET current_creative_index = current_creative_index + 1, updated_at = NOW() 
		WHERE campaign_id = $1
	`, campaignID)
	return err
}

// RecordESPSignal records an ESP deliverability signal
func (s *ObjectivesStore) RecordESPSignal(ctx context.Context, signal *ESPSignal) error {
	if signal.ID == uuid.Nil {
		signal.ID = uuid.New()
	}

	sampleMsgIDs, _ := json.Marshal(signal.SampleMessageIDs)

	query := `
		INSERT INTO mailing_esp_signals (
			id, campaign_id, organization_id, esp_type, signal_type,
			isp, receiving_domain, signal_count, sample_message_ids,
			bounce_class, error_code, error_message,
			interval_start, interval_end,
			ai_interpretation, ai_severity, recommended_action,
			action_taken, action_taken_at, created_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, NOW()
		)
	`

	_, err := s.db.ExecContext(ctx, query,
		signal.ID, signal.CampaignID, signal.OrganizationID, signal.ESPType, signal.SignalType,
		objNullString(signal.ISP), objNullString(signal.ReceivingDomain), signal.SignalCount, sampleMsgIDs,
		objNullString(signal.BounceClass), objNullString(signal.ErrorCode), objNullString(signal.ErrorMessage),
		signal.IntervalStart, signal.IntervalEnd,
		objNullString(signal.AIInterpretation), objNullString(signal.AISeverity), objNullString(signal.RecommendedAction),
		signal.ActionTaken, signal.ActionTakenAt,
	)

	return err
}

// GetRecentSignals gets recent ESP signals for a campaign
func (s *ObjectivesStore) GetRecentSignals(ctx context.Context, campaignID uuid.UUID, since time.Time) ([]ESPSignal, error) {
	query := `
		SELECT 
			id, campaign_id, organization_id, esp_type, signal_type,
			isp, receiving_domain, signal_count, sample_message_ids,
			bounce_class, error_code, error_message,
			interval_start, interval_end,
			ai_interpretation, ai_severity, recommended_action,
			action_taken, action_taken_at, created_at
		FROM mailing_esp_signals
		WHERE campaign_id = $1 AND created_at >= $2
		ORDER BY created_at DESC
	`

	rows, err := s.db.QueryContext(ctx, query, campaignID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var signals []ESPSignal
	for rows.Next() {
		var sig ESPSignal
		var isp, domain, bounceClass, errorCode, errorMsg sql.NullString
		var aiInterp, aiSev, recAction sql.NullString
		var actionTakenAt sql.NullTime
		var sampleMsgIDs []byte

		err := rows.Scan(
			&sig.ID, &sig.CampaignID, &sig.OrganizationID, &sig.ESPType, &sig.SignalType,
			&isp, &domain, &sig.SignalCount, &sampleMsgIDs,
			&bounceClass, &errorCode, &errorMsg,
			&sig.IntervalStart, &sig.IntervalEnd,
			&aiInterp, &aiSev, &recAction,
			&sig.ActionTaken, &actionTakenAt, &sig.CreatedAt,
		)
		if err != nil {
			return nil, err
		}

		sig.ISP = isp.String
		sig.ReceivingDomain = domain.String
		sig.BounceClass = bounceClass.String
		sig.ErrorCode = errorCode.String
		sig.ErrorMessage = errorMsg.String
		sig.AIInterpretation = aiInterp.String
		sig.AISeverity = aiSev.String
		sig.RecommendedAction = recAction.String
		if actionTakenAt.Valid {
			sig.ActionTakenAt = &actionTakenAt.Time
		}
		if len(sampleMsgIDs) > 0 {
			sig.SampleMessageIDs = sampleMsgIDs
		}

		signals = append(signals, sig)
	}

	return signals, nil
}

// GetSignalSummary gets aggregated signal counts by type for a campaign
func (s *ObjectivesStore) GetSignalSummary(ctx context.Context, campaignID uuid.UUID, since time.Time) (map[string]int, error) {
	query := `
		SELECT signal_type, SUM(signal_count) as total
		FROM mailing_esp_signals
		WHERE campaign_id = $1 AND created_at >= $2
		GROUP BY signal_type
	`

	rows, err := s.db.QueryContext(ctx, query, campaignID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	summary := make(map[string]int)
	for rows.Next() {
		var signalType string
		var total int
		if err := rows.Scan(&signalType, &total); err != nil {
			return nil, err
		}
		summary[signalType] = total
	}

	return summary, nil
}

// LogOptimization logs an AI optimization decision
func (s *ObjectivesStore) LogOptimization(ctx context.Context, log *CampaignOptimizationLog) error {
	if log.ID == uuid.Nil {
		log.ID = uuid.New()
	}

	triggerMetrics, _ := json.Marshal(log.TriggerMetrics)

	query := `
		INSERT INTO mailing_campaign_optimization_log (
			id, campaign_id, organization_id, optimization_type,
			trigger_reason, trigger_metrics, old_value, new_value,
			ai_reasoning, ai_confidence, applied, applied_at, created_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, NOW()
		)
	`

	_, err := s.db.ExecContext(ctx, query,
		log.ID, log.CampaignID, log.OrganizationID, log.OptimizationType,
		log.TriggerReason, triggerMetrics, objNullString(log.OldValue), objNullString(log.NewValue),
		objNullString(log.AIReasoning), log.AIConfidence, log.Applied, log.AppliedAt,
	)

	return err
}

// GetOptimizationLogs retrieves optimization logs for a campaign
func (s *ObjectivesStore) GetOptimizationLogs(ctx context.Context, campaignID uuid.UUID, since time.Time) ([]CampaignOptimizationLog, error) {
	query := `
		SELECT 
			id, campaign_id, organization_id, optimization_type,
			trigger_reason, trigger_metrics, old_value, new_value,
			ai_reasoning, ai_confidence, applied, applied_at,
			outcome_measured, outcome_positive, outcome_notes, created_at
		FROM mailing_campaign_optimization_log
		WHERE campaign_id = $1 AND created_at >= $2
		ORDER BY created_at DESC
		LIMIT 100
	`

	rows, err := s.db.QueryContext(ctx, query, campaignID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var logs []CampaignOptimizationLog
	for rows.Next() {
		var log CampaignOptimizationLog
		var oldValue, newValue, aiReasoning, outcomeNotes sql.NullString
		var aiConfidence sql.NullFloat64
		var appliedAt sql.NullTime
		var outcomePositive sql.NullBool
		var triggerMetrics []byte

		err := rows.Scan(
			&log.ID, &log.CampaignID, &log.OrganizationID, &log.OptimizationType,
			&log.TriggerReason, &triggerMetrics, &oldValue, &newValue,
			&aiReasoning, &aiConfidence, &log.Applied, &appliedAt,
			&log.OutcomeMeasured, &outcomePositive, &outcomeNotes, &log.CreatedAt,
		)
		if err != nil {
			return nil, err
		}

		log.OldValue = oldValue.String
		log.NewValue = newValue.String
		log.AIReasoning = aiReasoning.String
		log.OutcomeNotes = outcomeNotes.String
		if aiConfidence.Valid {
			log.AIConfidence = &aiConfidence.Float64
		}
		if appliedAt.Valid {
			log.AppliedAt = &appliedAt.Time
		}
		if outcomePositive.Valid {
			log.OutcomePositive = &outcomePositive.Bool
		}
		if len(triggerMetrics) > 0 {
			log.TriggerMetrics = triggerMetrics
		}

		logs = append(logs, log)
	}

	return logs, nil
}

// Helper functions for SQL null handling

func objNullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func objNullInt(i int) sql.NullInt64 {
	if i == 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(i), Valid: true}
}
