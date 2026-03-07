package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/engine"
)

func createPMTAWaveCampaign(
	ctx context.Context,
	tx *sql.Tx,
	db *sql.DB,
	orgID string,
	input engine.PMTACampaignInput,
	normalized pmtaNormalizedCampaign,
	audience pmtaAudiencePlan,
) (engine.PMTAWavePlanResult, error) {
	campaignID := uuid.New()
	scheduledAt := normalized.EarliestStart

	var profileID, fromEmail, fromName, replyTo sql.NullString
	if err := tx.QueryRowContext(ctx, `
		SELECT id, from_email, from_name, reply_email
		FROM mailing_sending_profiles
		WHERE organization_id = $1 AND vendor_type = 'pmta'
		  AND (sending_domain = $2 OR from_email LIKE '%@' || $2)
		  AND status = 'active'
		ORDER BY created_at DESC LIMIT 1
	`, orgID, input.SendingDomain).Scan(&profileID, &fromEmail, &fromName, &replyTo); err != nil && err != sql.ErrNoRows {
		return engine.PMTAWavePlanResult{}, fmt.Errorf("resolve sending profile: %w", err)
	}

	resolvedFromName := ""
	if fromName.Valid && fromName.String != "" {
		resolvedFromName = fromName.String
	}
	if len(input.Variants) > 0 && input.Variants[0].FromName != "" {
		resolvedFromName = input.Variants[0].FromName
	}
	resolvedFromEmail := ""
	if fromEmail.Valid {
		resolvedFromEmail = fromEmail.String
	}

	maxRecipients := audience.SelectedTotal
	quotaPayload, _ := json.Marshal(map[string]interface{}{
		"target_isps":        input.TargetISPs,
		"sending_domain":     input.SendingDomain,
		"throttle_strategy":  input.ThrottleStrategy,
		"isp_quotas":         input.ISPQuotas,
		"randomize_audience": input.RandomizeAudience,
		"isp_plans":          input.ISPPlans,
		"execution_mode":     pmtaExecutionModeWave,
	})
	inclusionIDs := resolveListNamesToIDs(ctx, db, orgID, input.InclusionLists)
	exclusionIDs := resolveListNamesToIDs(ctx, db, orgID, input.ExclusionLists)
	inclusionListsJSON, _ := json.Marshal(inclusionIDs)
	suppressionListsJSON, _ := json.Marshal(exclusionIDs)

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO mailing_campaigns (
			id, organization_id, name, status, scheduled_at,
			from_name, from_email, reply_to, subject, preview_text, html_content,
			sending_profile_id, esp_quotas, isp_quotas, list_ids, suppression_list_ids,
			max_recipients, send_type, timezone, execution_mode, total_recipients, queued_count,
			created_at, updated_at
		) VALUES (
			$1, $2, $3, 'scheduled', $4,
			$5, $6, $7, $8, $9, $10,
			$11, $12, $13, $14, $15,
			$16, 'blast', $17, $18, $19, 0,
			NOW(), NOW()
		)
	`, campaignID, orgID, input.Name, scheduledAt,
		resolvedFromName, resolvedFromEmail, replyTo,
		input.Variants[0].Subject, input.Variants[0].PreviewText, input.Variants[0].HTMLContent,
		nullUUID(profileID.String), quotaPayload, quotaPayload, inclusionListsJSON, suppressionListsJSON,
		maxRecipients, firstPlanTimezone(normalized), pmtaExecutionModeWave, audience.SelectedTotal,
	); err != nil {
		return engine.PMTAWavePlanResult{}, fmt.Errorf("insert wave campaign: %w", err)
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
		`, planID, campaignID, orgID, plan.ISP, input.SendingDomain, nullUUID(profileID.String),
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
