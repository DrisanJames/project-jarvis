package worker

import (
	"context"
	"database/sql"
	"time"

	"github.com/google/uuid"
)

type campaignVariant struct {
	VariantName string
	FromName    string
	Subject     string
	HTMLContent string
}

// EnqueuePMTAWave materializes one due PMTA wave into the existing recipient queue.
func EnqueuePMTAWave(ctx context.Context, db *sql.DB, waveID string) (int, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var (
		campaignID         uuid.UUID
		ispPlanID          uuid.UUID
		waveStatus         string
		campaignStatus     string
		planStatus         string
		scheduledAt        time.Time
		plannedRecipients  int
		enqueuedRecipients int
	)
	err = tx.QueryRowContext(ctx, `
		SELECT w.campaign_id, w.isp_plan_id, w.status, COALESCE(c.status, 'draft'),
		       COALESCE(p.status, 'planned'), w.scheduled_at, w.planned_recipients, w.enqueued_recipients
		FROM mailing_campaign_waves w
		JOIN mailing_campaigns c ON c.id = w.campaign_id
		JOIN mailing_campaign_isp_plans p ON p.id = w.isp_plan_id
		WHERE w.id = $1
		FOR UPDATE
	`, waveID).Scan(&campaignID, &ispPlanID, &waveStatus, &campaignStatus, &planStatus, &scheduledAt, &plannedRecipients, &enqueuedRecipients)
	if err != nil {
		return 0, err
	}

	switch waveStatus {
	case "completed", "cancelled", "failed", "dead_letter":
		return 0, tx.Commit()
	}
	if campaignStatus == "cancelled" || campaignStatus == "failed" || planStatus == "cancelled" || planStatus == "failed" || planStatus == "paused" {
		if _, err := tx.ExecContext(ctx, `
			UPDATE mailing_campaign_waves
			SET status = 'cancelled', updated_at = NOW()
			WHERE id = $1
		`, waveID); err != nil {
			return 0, err
		}
		return 0, tx.Commit()
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE mailing_campaign_waves
		SET status = 'enqueuing', started_at = COALESCE(started_at, NOW()), updated_at = NOW()
		WHERE id = $1
	`, waveID); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE mailing_campaigns
		SET status = 'sending', started_at = COALESCE(started_at, NOW()), updated_at = NOW()
		WHERE id = $1 AND status IN ('draft', 'scheduled', 'preparing')
	`, campaignID); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE mailing_campaign_isp_plans
		SET status = 'running', updated_at = NOW()
		WHERE id = $1 AND status IN ('planned', 'ready')
	`, ispPlanID); err != nil {
		return 0, err
	}

	remaining := plannedRecipients - enqueuedRecipients
	if remaining <= 0 {
		if _, err := tx.ExecContext(ctx, `
			UPDATE mailing_campaign_waves
			SET status = 'completed', completed_at = NOW(), updated_at = NOW()
			WHERE id = $1
		`, waveID); err != nil {
			return 0, err
		}
		return 0, tx.Commit()
	}

	var campaignFromName, campaignSubject, campaignHTML sql.NullString
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(from_name, ''), COALESCE(subject, ''), COALESCE(html_content, '')
		FROM mailing_campaigns WHERE id = $1
	`, campaignID).Scan(&campaignFromName, &campaignSubject, &campaignHTML); err != nil {
		return 0, err
	}

	variants, err := loadCampaignVariantsForWave(ctx, tx, campaignID.String(), campaignFromName.String, campaignSubject.String, campaignHTML.String)
	if err != nil {
		return 0, err
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT id, subscriber_id, email, recipient_isp, selection_rank,
		       audience_source_type, audience_source_id
		FROM mailing_campaign_plan_recipients
		WHERE isp_plan_id = $1
		  AND status = 'selected'
		ORDER BY selection_rank ASC
		LIMIT $2
		FOR UPDATE
	`, ispPlanID, remaining)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	type queueCandidate struct {
		recordID           uuid.UUID
		subscriberID       uuid.UUID
		email              string
		recipientISP       string
		selectionRank      int
		audienceSourceType string
		audienceSourceID   sql.NullString
	}

	var candidates []queueCandidate
	for rows.Next() {
		var rec queueCandidate
		if err := rows.Scan(&rec.recordID, &rec.subscriberID, &rec.email, &rec.recipientISP, &rec.selectionRank, &rec.audienceSourceType, &rec.audienceSourceID); err != nil {
			return 0, err
		}
		candidates = append(candidates, rec)
	}

	queuedCount := 0
	for idx, rec := range candidates {
		v := variants[idx%len(variants)]
		var sourceID interface{}
		if rec.audienceSourceID.Valid {
			parsed, err := uuid.Parse(rec.audienceSourceID.String)
			if err == nil {
				sourceID = parsed
			}
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO mailing_campaign_queue (
				id, campaign_id, subscriber_id, subject, html_content, plain_content,
				status, priority, scheduled_at, created_at, isp_plan_id, wave_id,
				recipient_isp, selection_rank, audience_source_type, audience_source_id
			) VALUES (
				$1, $2, $3, $4, $5, '',
				'queued', 5, $6, NOW(), $7, $8,
				$9, $10, $11, $12
			)
		`, uuid.New(), campaignID, rec.subscriberID, coalesceWaveValue(v.Subject, campaignSubject.String), coalesceWaveValue(v.HTMLContent, campaignHTML.String),
			scheduledAt, ispPlanID, waveID, rec.recipientISP, rec.selectionRank, rec.audienceSourceType, sourceID,
		); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE mailing_campaign_plan_recipients
			SET status = 'queued', queued_at = NOW(), wave_id = $2
			WHERE id = $1
		`, rec.recordID, waveID); err != nil {
			return 0, err
		}
		queuedCount++
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE mailing_campaign_waves
		SET enqueued_recipients = enqueued_recipients + $2,
		    status = 'completed',
		    completed_at = NOW(),
		    updated_at = NOW()
		WHERE id = $1
	`, waveID, queuedCount); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE mailing_campaign_isp_plans
		SET enqueued_count = enqueued_count + $2,
		    status = CASE
		        WHEN audience_selected_count <= enqueued_count + $2 THEN 'completed'
		        ELSE 'running'
		    END,
		    updated_at = NOW()
		WHERE id = $1
	`, ispPlanID, queuedCount); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE mailing_campaigns
		SET queued_count = queued_count + $2, updated_at = NOW()
		WHERE id = $1
	`, campaignID, queuedCount); err != nil {
		return 0, err
	}

	return queuedCount, tx.Commit()
}

func loadCampaignVariantsForWave(ctx context.Context, tx *sql.Tx, campaignID, fallbackFromName, fallbackSubject, fallbackHTML string) ([]campaignVariant, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT COALESCE(v.variant_name, ''),
		       COALESCE(NULLIF(v.from_name, ''), $2),
		       COALESCE(NULLIF(v.subject, ''), $3),
		       COALESCE(NULLIF(v.html_content, ''), $4)
		FROM mailing_ab_variants v
		JOIN mailing_ab_tests t ON t.id = v.test_id
		WHERE t.campaign_id = $1
		ORDER BY v.variant_name ASC
	`, campaignID, fallbackFromName, fallbackSubject, fallbackHTML)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var variants []campaignVariant
	for rows.Next() {
		var v campaignVariant
		if err := rows.Scan(&v.VariantName, &v.FromName, &v.Subject, &v.HTMLContent); err != nil {
			return nil, err
		}
		variants = append(variants, v)
	}
	if len(variants) == 0 {
		variants = append(variants, campaignVariant{
			VariantName: "A",
			FromName:    fallbackFromName,
			Subject:     fallbackSubject,
			HTMLContent: fallbackHTML,
		})
	}
	return variants, nil
}

func coalesceWaveValue(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}
