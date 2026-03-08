package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/engine"
)

type pmtaCampaignProfile struct {
	ProfileID sql.NullString
	FromEmail sql.NullString
	FromName  sql.NullString
	ReplyTo   sql.NullString
}

func createPMTAWaveCampaign(
	ctx context.Context,
	tx *sql.Tx,
	db *sql.DB,
	orgID string,
	input engine.PMTACampaignInput,
	normalized pmtaNormalizedCampaign,
	audience pmtaAudiencePlan,
) (engine.PMTAWavePlanResult, error) {
	campaignID, reusingDraft, err := resolvePMTACampaignIdentity(ctx, tx, orgID, input.CampaignID)
	if err != nil {
		return engine.PMTAWavePlanResult{}, err
	}
	scheduledAt := normalized.EarliestStart

	profile, err := resolvePMTASendingProfile(ctx, tx, orgID, input.SendingDomain)
	if err != nil {
		return engine.PMTAWavePlanResult{}, err
	}

	resolvedFromName := ""
	if profile.FromName.Valid && profile.FromName.String != "" {
		resolvedFromName = profile.FromName.String
	}
	if len(input.Variants) > 0 && input.Variants[0].FromName != "" {
		resolvedFromName = input.Variants[0].FromName
	}
	resolvedFromEmail := ""
	if profile.FromEmail.Valid {
		resolvedFromEmail = profile.FromEmail.String
	}

	maxRecipients := audience.SelectedTotal
	quotaPayload, _ := json.Marshal(buildPMTACampaignQuotaPayload(input))
	configJSON, _ := json.Marshal(pmtaCampaignConfig{
		CampaignInput: withCampaignID(input, campaignID.String()),
	})
	inclusionIDs := resolveListNamesToIDs(ctx, db, orgID, input.InclusionLists)
	exclusionIDs := resolveListNamesToIDs(ctx, db, orgID, input.ExclusionLists)
	inclusionListsJSON, _ := json.Marshal(inclusionIDs)
	suppressionListsJSON, _ := json.Marshal(exclusionIDs)
	suppressionSegmentsJSON, _ := json.Marshal(input.ExclusionSegments)

	if reusingDraft {
		if err := clearPMTACampaignChildren(ctx, tx, campaignID.String()); err != nil {
			return engine.PMTAWavePlanResult{}, err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE mailing_campaigns
			SET name = $2,
			    status = 'scheduled',
			    scheduled_at = $3,
			    from_name = $4,
			    from_email = $5,
			    reply_to = $6,
			    subject = $7,
			    preview_text = $8,
			    html_content = $9,
			    sending_profile_id = $10,
			    esp_quotas = $11,
			    isp_quotas = $12,
			    list_ids = $13,
			    suppression_list_ids = $14,
			    suppression_segment_ids = $15,
			    max_recipients = $16,
			    send_type = 'blast',
			    timezone = $17,
			    execution_mode = $18,
			    total_recipients = $19,
			    pmta_config = $20,
			    queued_count = 0,
			    started_at = NULL,
			    completed_at = NULL,
			    updated_at = NOW()
			WHERE id = $1 AND organization_id = $21
		`, campaignID, input.Name, scheduledAt,
			resolvedFromName, resolvedFromEmail, profile.ReplyTo,
			input.Variants[0].Subject, input.Variants[0].PreviewText, input.Variants[0].HTMLContent,
			nullUUID(profile.ProfileID.String), quotaPayload, quotaPayload, inclusionListsJSON, suppressionListsJSON,
			suppressionSegmentsJSON, maxRecipients, firstPlanTimezone(normalized), pmtaExecutionModeWave,
			audience.SelectedTotal, configJSON, orgID,
		); err != nil {
			return engine.PMTAWavePlanResult{}, fmt.Errorf("update wave campaign: %w", err)
		}
	} else {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO mailing_campaigns (
				id, organization_id, name, status, scheduled_at,
				from_name, from_email, reply_to, subject, preview_text, html_content,
				sending_profile_id, esp_quotas, isp_quotas, list_ids, suppression_list_ids, suppression_segment_ids,
				max_recipients, send_type, timezone, execution_mode, total_recipients, queued_count,
				pmta_config, created_at, updated_at
			) VALUES (
				$1, $2, $3, 'scheduled', $4,
				$5, $6, $7, $8, $9, $10,
				$11, $12, $13, $14, $15, $16,
				$17, 'blast', $18, $19, $20, 0,
				$21, NOW(), NOW()
			)
		`, campaignID, orgID, input.Name, scheduledAt,
			resolvedFromName, resolvedFromEmail, profile.ReplyTo,
			input.Variants[0].Subject, input.Variants[0].PreviewText, input.Variants[0].HTMLContent,
			nullUUID(profile.ProfileID.String), quotaPayload, quotaPayload, inclusionListsJSON, suppressionListsJSON,
			suppressionSegmentsJSON, maxRecipients, firstPlanTimezone(normalized), pmtaExecutionModeWave, audience.SelectedTotal,
			configJSON,
		); err != nil {
			return engine.PMTAWavePlanResult{}, fmt.Errorf("insert wave campaign: %w", err)
		}
	}

	if err := insertABVariants(ctx, tx, orgID, campaignID.String(), input); err != nil {
		return engine.PMTAWavePlanResult{}, err
	}

	planResults := make([]map[string]any, 0, len(normalized.Plans))
	waveResults := make([]map[string]any, 0)
	for _, plan := range normalized.Plans {
		planID := uuid.New()
		selectedCount := audience.CountsByISP[plan.ISP]
		planSnapshot, _ := json.Marshal(map[string]any{
			"isp":                plan.ISP,
			"quota":              plan.Quota,
			"randomize_audience": plan.RandomizeAudience,
			"throttle_strategy":  plan.ThrottleStrategy,
			"timezone":           plan.Timezone,
			"cadence":            plan.Cadence,
			"time_spans":         plan.TimeSpans,
		})

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO mailing_campaign_isp_plans (
				id, campaign_id, organization_id, isp, sending_domain, sending_profile_id,
				quota, randomize_audience, throttle_strategy, selection_strategy, priority_strategy,
				timezone, status, audience_estimated_count, audience_selected_count, config_snapshot,
				created_at, updated_at
			) VALUES (
				$1, $2, $3, $4, $5, $6,
				$7, $8, $9, 'priority_first', 'selection_rank',
				$10, 'ready', $11, $12, $13,
				NOW(), NOW()
			)
		`, planID, campaignID, orgID, plan.ISP, input.SendingDomain, nullUUID(profile.ProfileID.String),
			plan.Quota, plan.RandomizeAudience, plan.ThrottleStrategy,
			plan.Timezone, selectedCount, selectedCount, planSnapshot,
		); err != nil {
			return engine.PMTAWavePlanResult{}, fmt.Errorf("insert isp plan %s: %w", plan.ISP, err)
		}

		for idx, span := range plan.TimeSpans {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO mailing_campaign_isp_time_spans (
					id, isp_plan_id, campaign_id, span_type, start_at, end_at, timezone, source, sort_order, created_at
				) VALUES ($1, $2, $3, 'absolute', $4, $5, $6, $7, $8, NOW())
			`, uuid.New(), planID, campaignID, span.StartAt, span.EndAt, plan.Timezone, span.Source, idx); err != nil {
				return engine.PMTAWavePlanResult{}, fmt.Errorf("insert time span for %s: %w", plan.ISP, err)
			}
		}

		recipients := audience.RecipientsByISP[plan.ISP]
		for _, rec := range recipients {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO mailing_campaign_plan_recipients (
					id, campaign_id, isp_plan_id, subscriber_id, email, recipient_isp,
					selection_rank, audience_source_type, audience_source_id, status, created_at
				) VALUES (
					$1, $2, $3, $4, $5, $6,
					$7, $8, $9, 'selected', NOW()
				)
			`, uuid.New(), campaignID, planID, mustUUID(rec.SubscriberID), rec.Email, rec.ISP,
				rec.SelectionRank, rec.SourceType, parseNullableUUID(rec.SourceID),
			); err != nil {
				return engine.PMTAWavePlanResult{}, fmt.Errorf("insert plan recipient for %s: %w", plan.ISP, err)
			}
		}

		waves := buildPMTAWaveSpecs(campaignID.String(), plan, selectedCount)
		for _, wave := range waves {
			waveID := uuid.New()
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO mailing_campaign_waves (
					id, campaign_id, isp_plan_id, wave_number, scheduled_at, window_start_at, window_end_at,
					cadence_minutes, batch_size, planned_recipients, status, idempotency_key,
					created_at, updated_at
				) VALUES (
					$1, $2, $3, $4, $5, $6, $7,
					$8, $9, $10, 'planned', $11,
					NOW(), NOW()
				)
			`, waveID, campaignID, planID, wave.WaveNumber, wave.ScheduledAt, wave.WindowStartAt, wave.WindowEndAt,
				wave.CadenceMinutes, wave.BatchSize, wave.PlannedRecipients, wave.IdempotencyKey,
			); err != nil {
				return engine.PMTAWavePlanResult{}, fmt.Errorf("insert wave for %s: %w", plan.ISP, err)
			}
			waveResults = append(waveResults, map[string]any{
				"id":                 waveID.String(),
				"isp":                plan.ISP,
				"wave_number":        wave.WaveNumber,
				"scheduled_at":       wave.ScheduledAt,
				"window_start_at":    wave.WindowStartAt,
				"window_end_at":      wave.WindowEndAt,
				"planned_recipients": wave.PlannedRecipients,
				"batch_size":         wave.BatchSize,
				"cadence_minutes":    wave.CadenceMinutes,
			})
		}

		planResults = append(planResults, map[string]any{
			"id":                  planID.String(),
			"isp":                 plan.ISP,
			"quota":               plan.Quota,
			"timezone":            plan.Timezone,
			"randomize_audience":  plan.RandomizeAudience,
			"throttle_strategy":   plan.ThrottleStrategy,
			"selected_recipients": selectedCount,
			"time_span_count":     len(plan.TimeSpans),
			"wave_count":          len(waves),
		})
	}

	return engine.PMTAWavePlanResult{
		CampaignID:    campaignID.String(),
		Name:          input.Name,
		Status:        "scheduled",
		SendMode:      normalized.SendMode,
		SendsAt:       &scheduledAt,
		TargetISPs:    normalized.TargetISPs,
		TotalAudience: audience.SelectedTotal,
		VariantCount:  len(input.Variants),
		ISPPlans:      planResults,
		InitialWaves:  waveResults,
		Assumptions:   normalized.Assumptions,
		LegacyInput:   normalized.LegacyInput,
	}, nil
}

type pmtaCampaignConfig struct {
	CampaignInput engine.PMTACampaignInput `json:"campaign_input"`
	ScheduleMode  string                   `json:"schedule_mode,omitempty"`
}

func loadLatestPMTADraft(ctx context.Context, db *sql.DB, orgID string) (engine.PMTACampaignDraftResult, error) {
	var (
		campaignID string
		name       string
		status     string
		updatedAt  time.Time
		configJSON []byte
	)

	if err := db.QueryRowContext(ctx, `
		SELECT id::text, name, status, updated_at, COALESCE(pmta_config, '{}'::jsonb)::text
		FROM mailing_campaigns
		WHERE organization_id = $1
		  AND status = 'draft'
		  AND execution_mode = $2
		ORDER BY updated_at DESC, created_at DESC
		LIMIT 1
	`, orgID, pmtaExecutionModeWave).Scan(&campaignID, &name, &status, &updatedAt, &configJSON); err != nil {
		return engine.PMTACampaignDraftResult{}, err
	}

	var cfg pmtaCampaignConfig
	if len(configJSON) > 0 {
		_ = json.Unmarshal(configJSON, &cfg)
	}
	cfg.CampaignInput = withCampaignID(cfg.CampaignInput, campaignID)

	return engine.PMTACampaignDraftResult{
		CampaignID:    campaignID,
		Name:          firstNonEmptyDraftName(cfg.CampaignInput.Name, name),
		Status:        status,
		ScheduleMode:  normalizeDraftScheduleMode(cfg.ScheduleMode),
		UpdatedAt:     updatedAt,
		CampaignInput: cfg.CampaignInput,
	}, nil
}

func savePMTADraftCampaign(
	ctx context.Context,
	db *sql.DB,
	orgID string,
	draft engine.PMTACampaignDraftInput,
) (engine.PMTACampaignDraftResult, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return engine.PMTACampaignDraftResult{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	campaignID, _, err := resolvePMTACampaignIdentity(ctx, tx, orgID, draft.CampaignInput.CampaignID)
	if err != nil {
		return engine.PMTACampaignDraftResult{}, err
	}

	profile, err := resolvePMTASendingProfile(ctx, tx, orgID, draft.CampaignInput.SendingDomain)
	if err != nil {
		return engine.PMTACampaignDraftResult{}, err
	}

	input := withCampaignID(draft.CampaignInput, campaignID.String())
	config := pmtaCampaignConfig{
		CampaignInput: input,
		ScheduleMode:  normalizeDraftScheduleMode(draft.ScheduleMode),
	}
	configJSON, _ := json.Marshal(config)
	quotaPayload, _ := json.Marshal(buildPMTACampaignQuotaPayload(input))

	inclusionIDs := resolveListNamesToIDs(ctx, db, orgID, input.InclusionLists)
	exclusionIDs := resolveListNamesToIDs(ctx, db, orgID, input.ExclusionLists)
	inclusionListsJSON, _ := json.Marshal(inclusionIDs)
	suppressionListsJSON, _ := json.Marshal(exclusionIDs)
	suppressionSegmentsJSON, _ := json.Marshal(input.ExclusionSegments)

	resolvedFromName := ""
	if profile.FromName.Valid && profile.FromName.String != "" {
		resolvedFromName = profile.FromName.String
	}
	resolvedFromEmail := ""
	if profile.FromEmail.Valid {
		resolvedFromEmail = profile.FromEmail.String
	}
	if len(input.Variants) > 0 && strings.TrimSpace(input.Variants[0].FromName) != "" {
		resolvedFromName = input.Variants[0].FromName
	}

	subject := ""
	previewText := ""
	htmlContent := ""
	if len(input.Variants) > 0 {
		subject = input.Variants[0].Subject
		previewText = input.Variants[0].PreviewText
		htmlContent = input.Variants[0].HTMLContent
	}

	scheduledAt := derivePMTADraftScheduledAt(input)
	timezone := strings.TrimSpace(input.Timezone)
	if timezone == "" {
		timezone = "UTC"
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO mailing_campaigns (
			id, organization_id, name, status, scheduled_at,
			from_name, from_email, reply_to, subject, preview_text, html_content,
			sending_profile_id, esp_quotas, isp_quotas, list_ids, suppression_list_ids, suppression_segment_ids,
			send_type, timezone, execution_mode, total_recipients, queued_count,
			pmta_config, created_at, updated_at
		) VALUES (
			$1, $2, $3, 'draft', $4,
			$5, $6, $7, $8, $9, $10,
			$11, $12, $13, $14, $15, $16,
			'blast', $17, $18, 0, 0,
			$19, NOW(), NOW()
		)
		ON CONFLICT (id) DO UPDATE SET
			name = EXCLUDED.name,
			status = 'draft',
			scheduled_at = EXCLUDED.scheduled_at,
			from_name = EXCLUDED.from_name,
			from_email = EXCLUDED.from_email,
			reply_to = EXCLUDED.reply_to,
			subject = EXCLUDED.subject,
			preview_text = EXCLUDED.preview_text,
			html_content = EXCLUDED.html_content,
			sending_profile_id = EXCLUDED.sending_profile_id,
			esp_quotas = EXCLUDED.esp_quotas,
			isp_quotas = EXCLUDED.isp_quotas,
			list_ids = EXCLUDED.list_ids,
			suppression_list_ids = EXCLUDED.suppression_list_ids,
			suppression_segment_ids = EXCLUDED.suppression_segment_ids,
			send_type = EXCLUDED.send_type,
			timezone = EXCLUDED.timezone,
			execution_mode = EXCLUDED.execution_mode,
			total_recipients = 0,
			queued_count = 0,
			started_at = NULL,
			completed_at = NULL,
			pmta_config = EXCLUDED.pmta_config,
			updated_at = NOW()
	`, campaignID, orgID, input.Name, scheduledAt,
		resolvedFromName, resolvedFromEmail, profile.ReplyTo, subject, previewText, htmlContent,
		nullUUID(profile.ProfileID.String), quotaPayload, quotaPayload, inclusionListsJSON, suppressionListsJSON, suppressionSegmentsJSON,
		timezone, pmtaExecutionModeWave, configJSON,
	)
	if err != nil {
		return engine.PMTACampaignDraftResult{}, fmt.Errorf("save PMTA draft: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return engine.PMTACampaignDraftResult{}, fmt.Errorf("commit PMTA draft: %w", err)
	}

	return engine.PMTACampaignDraftResult{
		CampaignID:    campaignID.String(),
		Name:          input.Name,
		Status:        "draft",
		ScheduleMode:  normalizeDraftScheduleMode(draft.ScheduleMode),
		UpdatedAt:     time.Now().UTC(),
		CampaignInput: input,
	}, nil
}

func insertABVariants(ctx context.Context, tx *sql.Tx, orgID, campaignID string, input engine.PMTACampaignInput) error {
	testID := uuid.New().String()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO mailing_ab_tests (
			id, organization_id, campaign_id, name, test_type,
			test_sample_percent, winner_metric, status,
			created_at, updated_at
		) VALUES ($1, $2, $3, $4, 'content', 100, 'open_rate', 'testing', NOW(), NOW())
	`, testID, orgID, campaignID, input.Name+" A/B Test"); err != nil {
		return fmt.Errorf("create ab test: %w", err)
	}

	for _, v := range input.Variants {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO mailing_ab_variants (
				id, test_id, variant_name, from_name, subject, html_content,
				split_percent, created_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())
		`, uuid.New().String(), testID, v.VariantName, v.FromName, v.Subject, v.HTMLContent, v.SplitPercent); err != nil {
			return fmt.Errorf("create ab variant %s: %w", v.VariantName, err)
		}
	}
	return nil
}

func firstPlanTimezone(normalized pmtaNormalizedCampaign) string {
	if len(normalized.Plans) == 0 {
		return "UTC"
	}
	if strings.TrimSpace(normalized.Plans[0].Timezone) == "" {
		return "UTC"
	}
	return normalized.Plans[0].Timezone
}

func parseNullableUUID(raw string) interface{} {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return nil
	}
	return id
}

func nullUUID(raw string) interface{} {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return nil
	}
	return id
}

func resolvePMTACampaignIdentity(ctx context.Context, tx *sql.Tx, orgID, requestedCampaignID string) (uuid.UUID, bool, error) {
	requestedCampaignID = strings.TrimSpace(requestedCampaignID)
	if requestedCampaignID != "" {
		campaignID, err := uuid.Parse(requestedCampaignID)
		if err != nil {
			return uuid.Nil, false, fmt.Errorf("invalid campaign_id: %w", err)
		}

		var existingID uuid.UUID
		if err := tx.QueryRowContext(ctx, `
			SELECT id
			FROM mailing_campaigns
			WHERE id = $1
			  AND organization_id = $2
			  AND status = 'draft'
			  AND execution_mode = $3
			LIMIT 1
		`, campaignID, orgID, pmtaExecutionModeWave).Scan(&existingID); err != nil {
			if err == sql.ErrNoRows {
				return uuid.Nil, false, fmt.Errorf("PMTA draft %s was not found or is no longer editable", requestedCampaignID)
			}
			return uuid.Nil, false, fmt.Errorf("lookup draft campaign: %w", err)
		}
		return existingID, true, nil
	}

	var existingID uuid.UUID
	if err := tx.QueryRowContext(ctx, `
		SELECT id
		FROM mailing_campaigns
		WHERE organization_id = $1
		  AND status = 'draft'
		  AND execution_mode = $2
		ORDER BY updated_at DESC, created_at DESC
		LIMIT 1
	`, orgID, pmtaExecutionModeWave).Scan(&existingID); err == nil {
		return existingID, true, nil
	} else if err != sql.ErrNoRows {
		return uuid.Nil, false, fmt.Errorf("lookup latest draft campaign: %w", err)
	}

	return uuid.New(), false, nil
}

func resolvePMTASendingProfile(ctx context.Context, tx *sql.Tx, orgID, sendingDomain string) (pmtaCampaignProfile, error) {
	var profile pmtaCampaignProfile
	if err := tx.QueryRowContext(ctx, `
		SELECT id, from_email, from_name, reply_email
		FROM mailing_sending_profiles
		WHERE organization_id = $1 AND vendor_type = 'pmta'
		  AND (sending_domain = $2 OR from_email LIKE '%@' || $2)
		  AND status = 'active'
		ORDER BY created_at DESC LIMIT 1
	`, orgID, sendingDomain).Scan(&profile.ProfileID, &profile.FromEmail, &profile.FromName, &profile.ReplyTo); err != nil && err != sql.ErrNoRows {
		return pmtaCampaignProfile{}, fmt.Errorf("resolve sending profile: %w", err)
	}
	return profile, nil
}

func clearPMTACampaignChildren(ctx context.Context, tx *sql.Tx, campaignID string) error {
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM mailing_ab_variants
		WHERE test_id IN (
			SELECT id FROM mailing_ab_tests WHERE campaign_id = $1
		)
	`, campaignID); err != nil {
		return fmt.Errorf("delete ab variants: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM mailing_ab_tests WHERE campaign_id = $1`, campaignID); err != nil {
		return fmt.Errorf("delete ab tests: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM mailing_campaign_isp_plans WHERE campaign_id = $1`, campaignID); err != nil {
		return fmt.Errorf("delete PMTA plan rows: %w", err)
	}
	return nil
}

func derivePMTADraftScheduledAt(input engine.PMTACampaignInput) interface{} {
	if input.ScheduledAt != nil {
		return input.ScheduledAt.UTC()
	}

	var earliest *time.Time
	for _, plan := range input.ISPPlans {
		for _, span := range plan.TimeSpans {
			if span.StartAt == nil {
				continue
			}
			startAt := span.StartAt.UTC()
			if earliest == nil || startAt.Before(*earliest) {
				earliest = &startAt
			}
		}
	}
	if earliest == nil {
		return nil
	}
	return *earliest
}

func buildPMTACampaignQuotaPayload(input engine.PMTACampaignInput) map[string]interface{} {
	return map[string]interface{}{
		"target_isps":        input.TargetISPs,
		"sending_domain":     input.SendingDomain,
		"throttle_strategy":  input.ThrottleStrategy,
		"isp_quotas":         input.ISPQuotas,
		"randomize_audience": input.RandomizeAudience,
		"isp_plans":          input.ISPPlans,
		"execution_mode":     pmtaExecutionModeWave,
	}
}

func normalizeDraftScheduleMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "per-isp":
		return "per-isp"
	default:
		return "quick"
	}
}

func withCampaignID(input engine.PMTACampaignInput, campaignID string) engine.PMTACampaignInput {
	input.CampaignID = campaignID
	return input
}

func firstNonEmptyDraftName(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return "PMTA Draft"
}
