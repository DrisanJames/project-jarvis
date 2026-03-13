package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/mailing"
)

// brandConfig holds per-brand settings for wave content generation.
type brandConfig struct {
	SendingDomain string
	BlogDomain    string
	BrandName     string
	FromName      string
	CampaignType  string
}

// knownBrands returns the configured brand roster.
func knownBrands() map[string]brandConfig {
	return map[string]brandConfig{
		"discountblog": {
			SendingDomain: "em.discountblog.com",
			BlogDomain:    "discountblog.com",
			BrandName:     "Discount Blog",
			FromName:      "Jamie @ Discount Blog",
			CampaignType:  "newsletter",
		},
		"quizfiesta": {
			SendingDomain: "em.quizfiesta.com",
			BlogDomain:    "quizfiesta.com",
			BrandName:     "QuizFiesta",
			FromName:      "QuizFiesta Team",
			CampaignType:  "trivia",
		},
	}
}

// HandleWaveContentTest generates structurally unique email content per wave
// using AI, then deploys a test campaign. Each brand is isolated — its own
// scrape, its own prompt, its own generation cycle.
//
// Query params:
//
//	mode=prompt   → scrapes blog, builds prompt, returns it for review
//	mode=generate → scrapes blog, generates via AI, deploys campaign
//	brand         → "discountblog", "quizfiesta", or "all" (default: all)
//	email         → test recipient (default: drisanjames@gmail.com)
//	waves         → number of waves per brand (default: 3)
func (s *PMTACampaignService) HandleWaveContentTest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := s.orgID

	mode := r.URL.Query().Get("mode")
	if mode == "" {
		mode = "prompt"
	}
	brandFilter := r.URL.Query().Get("brand")
	if brandFilter == "" {
		brandFilter = "all"
	}
	testEmail := r.URL.Query().Get("email")
	if testEmail == "" {
		testEmail = "drisanjames@gmail.com"
	}
	numWaves := 3
	if nw := r.URL.Query().Get("waves"); nw != "" {
		if v, err := strconv.Atoi(nw); err == nil && v > 0 && v <= 10 {
			numWaves = v
		}
	}

	anthropicKey := os.Getenv("ANTHROPIC_API_KEY")
	openaiKey := os.Getenv("OPENAI_API_KEY")
	if anthropicKey == "" && openaiKey == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{
			"error": "no AI API key configured (need ANTHROPIC_API_KEY or OPENAI_API_KEY)",
		})
		return
	}

	aiSvc := mailing.NewAIContentService(s.db, anthropicKey, openaiKey)
	waveGen := mailing.NewWaveContentGenerator(aiSvc)

	brands := knownBrands()
	var selectedBrands []brandConfig

	if brandFilter == "all" {
		for _, b := range brands {
			selectedBrands = append(selectedBrands, b)
		}
	} else {
		b, ok := brands[brandFilter]
		if !ok {
			respondJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("unknown brand %q — use discountblog, quizfiesta, or all", brandFilter),
			})
			return
		}
		selectedBrands = []brandConfig{b}
	}

	if mode == "prompt" {
		var brandPrompts []map[string]interface{}

		for _, b := range selectedBrands {
			log.Printf("[wave-content-test] scraping brand intelligence from %s", b.BlogDomain)
			brand := aiSvc.ScrapeBrandIntelligence(ctx, b.BlogDomain)

			req := mailing.WaveContentRequest{
				SendingDomain: b.SendingDomain,
				BrandName:     b.BrandName,
				NumWaves:      numWaves,
				CampaignType:  b.CampaignType,
				BrandInfo:     brand,
			}
			prompt := waveGen.BuildPrompt(req)

			brandPrompts = append(brandPrompts, map[string]interface{}{
				"brand_name":          b.BrandName,
				"from_name":          b.FromName,
				"sending_domain":     b.SendingDomain,
				"blog_domain":        b.BlogDomain,
				"campaign_type":      b.CampaignType,
				"prompt":             prompt,
				"prompt_length_chars": len(prompt),
				"brand_info":         brand,
			})
		}

		respondJSON(w, http.StatusOK, map[string]interface{}{
			"mode":       "prompt",
			"brands":     brandPrompts,
			"num_waves":  numWaves,
			"test_email": testEmail,
			"note":       "Review each brand's prompt above. When ready, call with mode=generate to create and deploy.",
		})
		return
	}

	if mode != "generate" {
		respondJSON(w, http.StatusBadRequest, map[string]string{
			"error": "mode must be 'prompt' or 'generate'",
		})
		return
	}

	testListID := ensureWaveTestRecipients(ctx, s.db, orgID, testEmail)
	if testListID == "" {
		respondJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "failed to create test recipient list",
		})
		return
	}

	var allBrandResults []map[string]interface{}
	baseTime := time.Now().Add(5 * time.Minute)
	cadenceMinutes := 3
	globalWaveOffset := 0

	for _, b := range selectedBrands {
		log.Printf("[wave-content-test] scraping brand intelligence from %s", b.BlogDomain)
		brand := aiSvc.ScrapeBrandIntelligence(ctx, b.BlogDomain)

		req := mailing.WaveContentRequest{
			SendingDomain: b.SendingDomain,
			BrandName:     b.BrandName,
			NumWaves:      numWaves,
			CampaignType:  b.CampaignType,
			BrandInfo:     brand,
		}

		log.Printf("[wave-content-test] generating %d wave variations for %s via AI...", numWaves, b.BrandName)
		variations, err := waveGen.Generate(ctx, req)
		if err != nil {
			allBrandResults = append(allBrandResults, map[string]interface{}{
				"brand": b.BrandName,
				"error": fmt.Sprintf("AI generation failed: %v", err),
			})
			continue
		}

		var profileID, fromEmail sql.NullString
		s.db.QueryRowContext(ctx, `
			SELECT id, from_email
			FROM mailing_sending_profiles
			WHERE organization_id = $1 AND vendor_type = 'pmta'
			  AND (sending_domain = $2 OR from_email LIKE '%%@' || $2)
			  AND status = 'active'
			ORDER BY created_at DESC LIMIT 1
		`, orgID, b.SendingDomain).Scan(&profileID, &fromEmail)

		var waveResults []map[string]interface{}
		for i, v := range variations {
			if i >= numWaves {
				break
			}

			campaignID := uuid.New().String()
			campaignName := fmt.Sprintf("%s Wave %d/%d", b.BrandName, i+1, numWaves)
			scheduledAt := baseTime.Add(time.Duration((globalWaveOffset+i)*cadenceMinutes) * time.Minute)

			fName := b.FromName
			fEmail := ""
			if fromEmail.Valid {
				fEmail = fromEmail.String
			}

			espQuotas, _ := json.Marshal(map[string]interface{}{
				"target_isps":    []map[string]string{{"name": "Gmail", "domain": "gmail.com"}},
				"sending_domain": b.SendingDomain,
			})
			inclusionListsJSON, _ := json.Marshal([]string{testListID})

			_, insertErr := s.db.ExecContext(ctx, `
				INSERT INTO mailing_campaigns (
					id, organization_id, name, status, scheduled_at,
					from_name, from_email, subject, preview_text, html_content,
					sending_profile_id, esp_quotas, list_ids,
					send_type, created_at, updated_at
				) VALUES (
					$1, $2, $3, 'scheduled', $4,
					$5, $6, $7, $8, $9,
					$10, $11, $12,
					'blast', NOW(), NOW()
				)
			`, campaignID, orgID, campaignName, scheduledAt,
				fName, fEmail, v.Subject, v.PreviewText, v.HTMLContent,
				profileID, string(espQuotas), string(inclusionListsJSON),
			)

			status := "scheduled"
			if insertErr != nil {
				status = "error: " + insertErr.Error()
				log.Printf("[wave-content-test] %s wave %d insert error: %v", b.BrandName, i+1, insertErr)
			}

			waveResults = append(waveResults, map[string]interface{}{
				"wave":          i + 1,
				"campaign_id":   campaignID,
				"campaign_name": campaignName,
				"scheduled_at":  scheduledAt.Format(time.RFC3339),
				"subject":       v.Subject,
				"preview_text":  v.PreviewText,
				"from_name":     fName,
				"html_length":   len(v.HTMLContent),
				"status":        status,
			})
		}

		globalWaveOffset += numWaves

		allBrandResults = append(allBrandResults, map[string]interface{}{
			"brand":      b.BrandName,
			"from_name":  b.FromName,
			"domain":     b.SendingDomain,
			"waves":      waveResults,
			"brand_info": brand,
		})
	}

	totalWaves := numWaves * len(selectedBrands)
	respondJSON(w, http.StatusCreated, map[string]interface{}{
		"message":        fmt.Sprintf("Deployed %d wave campaigns across %d brands", totalWaves, len(selectedBrands)),
		"test_email":     testEmail,
		"total_campaigns": totalWaves,
		"cadence_min":    cadenceMinutes,
		"brands":         allBrandResults,
	})
}

// ensureWaveTestRecipients creates or finds a test list with the given email.
func ensureWaveTestRecipients(ctx context.Context, db *sql.DB, orgID, email string) string {
	listName := "Wave Content Test List"

	var listID string
	err := db.QueryRowContext(ctx, `
		SELECT id::text FROM mailing_lists
		WHERE organization_id = $1 AND name = $2
		LIMIT 1
	`, orgID, listName).Scan(&listID)

	if err != nil {
		listID = uuid.New().String()
		_, err = db.ExecContext(ctx, `
			INSERT INTO mailing_lists (id, organization_id, name, description, status, created_at, updated_at)
			VALUES ($1, $2, $3, 'Auto-created for wave content testing', 'active', NOW(), NOW())
			ON CONFLICT DO NOTHING
		`, listID, orgID, listName)
		if err != nil {
			log.Printf("[wave-content-test] failed to create test list: %v", err)
			return ""
		}
	}

	db.ExecContext(ctx, `
		DELETE FROM mailing_list_subscribers
		WHERE list_id = $1
	`, listID)

	var actualSubID string
	db.QueryRowContext(ctx, `
		SELECT id::text FROM mailing_subscribers WHERE organization_id = $1 AND email = $2 LIMIT 1
	`, orgID, email).Scan(&actualSubID)

	if actualSubID == "" {
		actualSubID = uuid.New().String()
		db.ExecContext(ctx, `
			INSERT INTO mailing_subscribers (id, organization_id, email, first_name, last_name, status, engagement_score, subscribed_at, created_at, updated_at)
			VALUES ($1, $2, $3, 'James', 'Ventures', 'confirmed', 80, NOW(), NOW(), NOW())
			ON CONFLICT (organization_id, email) DO UPDATE SET status = 'confirmed', updated_at = NOW()
		`, actualSubID, orgID, email)

		db.QueryRowContext(ctx, `
			SELECT id::text FROM mailing_subscribers WHERE organization_id = $1 AND email = $2 LIMIT 1
		`, orgID, email).Scan(&actualSubID)
	}

	if actualSubID != "" {
		db.ExecContext(ctx, `
			INSERT INTO mailing_list_subscribers (list_id, subscriber_id, subscribed_at)
			VALUES ($1, $2, NOW())
			ON CONFLICT DO NOTHING
		`, listID, actualSubID)
	}

	var count int
	db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM mailing_list_subscribers WHERE list_id = $1
	`, listID).Scan(&count)
	log.Printf("[wave-content-test] test list %s has %d subscriber(s)", listID, count)

	return listID
}
