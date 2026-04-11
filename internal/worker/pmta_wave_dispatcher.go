package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/mailing"
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

	var campaignFromName, campaignFromEmail, campaignSubject, campaignHTML, campaignName, campaignPlain sql.NullString
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(from_name, ''), COALESCE(from_email, ''), COALESCE(subject, ''),
		       COALESCE(html_content, ''), COALESCE(name, ''), COALESCE(plain_content, '')
		FROM mailing_campaigns WHERE id = $1
	`, campaignID).Scan(&campaignFromName, &campaignFromEmail, &campaignSubject, &campaignHTML, &campaignName, &campaignPlain); err != nil {
		return 0, err
	}

	// Hash Fingerprint Diversification: use the campaign's own content directly.
	// Per-recipient HTML/subject mutations are applied in the queue insertion loop
	// to produce unique fingerprints without changing visible content.
	brandKey := deriveBrandKey(campaignFromEmail.String)
	_ = campaignFromName // from_name lives on the campaign; not needed per-recipient
	_ = campaignName     // campaign type detection reserved for future editorial path

	baseHTML := campaignHTML.String
	baseSubject := campaignSubject.String

	// URL sanitization still runs on the base content before per-recipient mutation
	sanitized := sanitizeVariantURLsAtDispatch([]campaignVariant{{HTMLContent: baseHTML}}, brandKey)
	baseHTML = sanitized[0].HTMLContent

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
	skippedCount := 0
	for _, rec := range candidates {
		// Phase D: last-second compliance guard — skip if subscriber is no longer active
		// or email is globally suppressed. This protects against unsubs/bounces that
		// occurred between audience snapshot and wave enqueue.
		var subStatus string
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(status, 'active') FROM mailing_subscribers WHERE id = $1`,
			rec.subscriberID,
		).Scan(&subStatus); err == nil && subStatus != "active" && subStatus != "confirmed" {
			tx.ExecContext(ctx, `UPDATE mailing_campaign_plan_recipients SET status = 'skipped' WHERE id = $1 AND status = 'selected'`, rec.recordID)
			skippedCount++
			continue
		}
		var suppExists bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM mailing_global_suppressions WHERE md5_hash = md5($1))`,
			strings.ToLower(rec.email),
		).Scan(&suppExists); err == nil && suppExists {
			tx.ExecContext(ctx, `UPDATE mailing_campaign_plan_recipients SET status = 'skipped' WHERE id = $1 AND status = 'selected'`, rec.recordID)
			skippedCount++
			continue
		}

		seed := computeMutationSeed(rec.subscriberID, waveID)
		recipientHTML := mutateHTMLHash(baseHTML, seed)
		recipientHTML = injectHoneypotLink(recipientHTML, rec.subscriberID.String())
		recipientSubject := mutateSubjectLine(baseSubject, seed, brandKey)

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
				$1, $2, $3, $4, $5, $13,
				'queued', 5, $6, NOW(), $7, $8,
				$9, $10, $11, $12
			)
		`, uuid.New(), campaignID, rec.subscriberID, recipientSubject, recipientHTML,
			scheduledAt, ispPlanID, waveID, rec.recipientISP, rec.selectionRank, rec.audienceSourceType, sourceID,
			campaignPlain.String,
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

	if skippedCount > 0 {
		log.Printf("[WaveEnqueue] wave %s: skipped %d recipients (Phase D compliance)", waveID, skippedCount)
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

// deriveBrandKey extracts the brand key from a from_email address.
// "hello@em.discountblog.com" → "discountblog"
// "hello@em.quizfiesta.com"   → "quizfiesta"
func deriveBrandKey(fromEmail string) string {
	at := strings.LastIndex(fromEmail, "@")
	if at < 0 {
		return ""
	}
	domain := strings.ToLower(fromEmail[at+1:])
	domain = strings.TrimPrefix(domain, "em.")
	domain = strings.TrimPrefix(domain, "m.")
	dot := strings.Index(domain, ".")
	if dot > 0 {
		return domain[:dot]
	}
	return domain
}

// detectCampaignType infers the campaign type from its name.
func detectCampaignType(name string) string {
	lower := strings.ToLower(name)
	if strings.Contains(lower, "welcome") {
		return "welcome"
	}
	if strings.Contains(lower, "trivia") || strings.Contains(lower, "quiz") {
		return "trivia"
	}
	return "newsletter"
}

// buildVariantFromEditorial takes cached editorial JSON and re-renders it into
// the campaign's own HTML template. If editorial JSON is available and the
// campaign HTML has placeholders, the editorial is merged in. Otherwise, only
// the subject/preview are varied.
func buildVariantFromEditorial(cached *CachedWaveContent, fallbackFromName, fallbackSubject, campaignHTML, brandKey string) campaignVariant {
	subject := coalesceWaveValue(cached.Variation.Subject, fallbackSubject)
	fromName := coalesceWaveValue(cached.Variation.FromName, fallbackFromName)

	htmlOut := campaignHTML
	if len(cached.EditorialJSON) > 0 && hasPlaceholders(campaignHTML) {
		var editorial mailing.WaveEditorialContent
		if err := json.Unmarshal(cached.EditorialJSON, &editorial); err == nil {
			req := mailing.WaveContentRequest{
				HTMLTemplate: campaignHTML,
				BrandName:    brandKey,
			}
			rendered := mailing.TemplateFillWithVariation([]mailing.WaveEditorialContent{editorial}, req)
			if len(rendered) > 0 {
				htmlOut = rendered[0].HTMLContent
				subject = coalesceWaveValue(rendered[0].Subject, subject)
			}
		} else {
			log.Printf("[wave-dispatch] failed to parse editorial JSON: %v", err)
		}
	} else if len(cached.EditorialJSON) == 0 && hasPlaceholders(campaignHTML) {
		log.Printf("[wave-dispatch] campaign has placeholders but no editorial JSON — using cached rendered HTML")
		htmlOut = cached.Variation.HTMLContent
	}

	return campaignVariant{
		VariantName: fmt.Sprintf("wave-%d", cached.Variation.WaveIndex),
		FromName:    fromName,
		Subject:     subject,
		HTMLContent: htmlOut,
	}
}

func hasPlaceholders(html string) bool {
	return strings.Contains(html, "{{INTRO}}") || strings.Contains(html, "{{ARTICLE_1_HEADLINE}}")
}

// brandURLFallbacks maps brand keys to their fallback redirect target.
// AI-generated article slugs that don't match any known path are rewritten
// to the fallback URL (typically the blog root) so users land on real content.
var brandURLFallbacks = map[string]struct {
	Domain     string
	Fallback   string
	KnownPaths []string
}{
	"historythinking": {
		Domain:   "historythinking.com",
		Fallback: "/blog",
		KnownPaths: []string{
			"/blog", "/privacy", "/terms", "/about", "/auth",
			"/blog/category/ancient-civilizations",
			"/blog/category/american-history",
			"/blog/category/cultural-history",
			"/blog/category/historical-figures",
			"/blog/category/medieval-world",
			"/blog/category/revolutionary-movements",
			"/blog/category/science-and-discovery",
			"/blog/category/world-wars",
		},
	},
}

// sanitizeVariantURLsAtDispatch rewrites hallucinated article URLs to the
// brand's blog root. AI sometimes fabricates article slugs that 404; this
// catches them at dispatch time and redirects to real content.
func sanitizeVariantURLsAtDispatch(variants []campaignVariant, brandKey string) []campaignVariant {
	rule, ok := brandURLFallbacks[brandKey]
	if !ok {
		return variants
	}

	baseURL := "https://" + rule.Domain
	re := regexp.MustCompile(`href="` + regexp.QuoteMeta(baseURL) + `/([^"]+)"`)

	for i := range variants {
		html := variants[i].HTMLContent
		replaced := false
		html = re.ReplaceAllStringFunc(html, func(match string) string {
			slug := re.FindStringSubmatch(match)
			if len(slug) < 2 {
				return match
			}
			path := "/" + slug[1]
			for _, kp := range rule.KnownPaths {
				if path == kp || strings.HasPrefix(path, kp+"/") {
					return match
				}
			}
			replaced = true
			return `href="` + baseURL + rule.Fallback + `"`
		})
		if replaced {
			variants[i].HTMLContent = html
			log.Printf("[wave-dispatch-urlfix] rewrote hallucinated URLs to %s%s for variant %s (brand: %s)",
				baseURL, rule.Fallback, variants[i].VariantName, brandKey)
		}
	}
	return variants
}
