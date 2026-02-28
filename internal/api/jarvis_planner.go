package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

func (j *JarvisOrchestrator) HandleAutonomousPlan(w http.ResponseWriter, r *http.Request) {
	log.Println("[Jarvis/Planner] Autonomous planning initiated...")

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	ownerEmails := make([]string, 0, len(ownerTestAccounts))
	for email := range ownerTestAccounts {
		ownerEmails = append(ownerEmails, email)
	}

	type offerCandidate struct {
		OfferID         string
		OfferName       string
		Clicks          int64
		Conversions     int64
		Revenue         float64
		EPC             float64
		CVR             float64
		HasSuppression  bool
		SuppressionID   string
		SuppressionName string
	}

	now := time.Now()
	startDate := now.AddDate(0, 0, -7)
	var candidates []offerCandidate

	if j.efClient != nil {
		report, err := j.efClient.GetEntityReportByOffer(ctx, startDate, now, nil)
		if err != nil {
			log.Printf("[Jarvis/Planner] Everflow offer report error: %v — using fallback", err)
		} else if report != nil && len(report.Table) > 0 {
			for _, row := range report.Table {
				offerID := ""
				offerName := ""

				if len(row.Columns) > 0 {
					offerID = row.Columns[0].ID
					offerName = row.Columns[0].Label
				}

				clicks := row.Reporting.GrossClick
				conversions := row.Reporting.Conversions
				revenue := row.Reporting.Revenue

				if clicks > 0 || conversions > 0 {
					epc := 0.0
					if clicks > 0 {
						epc = revenue / float64(clicks)
					}
					cvr := 0.0
					if clicks > 0 {
						cvr = float64(conversions) / float64(clicks) * 100
					}
					candidates = append(candidates, offerCandidate{
						OfferID:     offerID,
						OfferName:   offerName,
						Clicks:      clicks,
						Conversions: conversions,
						Revenue:     revenue,
						EPC:         epc,
						CVR:         cvr,
					})
				}
			}
		}
	}

	suppressionRows, err := j.db.QueryContext(ctx,
		"SELECT id, name, description, entry_count FROM mailing_suppression_lists WHERE entry_count > 0 ORDER BY entry_count DESC")
	if err != nil {
		log.Printf("[Jarvis/Planner] Error querying suppression lists: %v", err)
	}

	type suppressionList struct {
		ID          string
		Name        string
		Description string
		EntryCount  int
	}
	var availableSuppressions []suppressionList
	if suppressionRows != nil {
		defer suppressionRows.Close()
		for suppressionRows.Next() {
			var s suppressionList
			var desc sql.NullString
			if err := suppressionRows.Scan(&s.ID, &s.Name, &desc, &s.EntryCount); err == nil {
				s.Description = desc.String
				availableSuppressions = append(availableSuppressions, s)
			}
		}
	}

	for i := range candidates {
		offerLower := strings.ToLower(candidates[i].OfferName)
		for _, sl := range availableSuppressions {
			slNameLower := strings.ToLower(sl.Name)
			slDescLower := strings.ToLower(sl.Description)
			if strings.Contains(slNameLower, offerLower) || strings.Contains(offerLower, slNameLower) ||
				strings.Contains(slDescLower, offerLower) ||
				strings.Contains(slNameLower, candidates[i].OfferID) {
				candidates[i].HasSuppression = true
				candidates[i].SuppressionID = sl.ID
				candidates[i].SuppressionName = sl.Name
				break
			}
		}
	}

	sort.Slice(candidates, func(i, k int) bool {
		return candidates[i].EPC > candidates[k].EPC
	})

	var selectedOffers []offerCandidate
	var rejectedOffers []string
	for _, c := range candidates {
		if len(selectedOffers) >= 2 {
			break
		}
		if !c.HasSuppression {
			rejectedOffers = append(rejectedOffers, fmt.Sprintf("%s (%s) — NO SUPPRESSION LIST", c.OfferName, c.OfferID))
			continue
		}
		selectedOffers = append(selectedOffers, c)
	}

	if len(selectedOffers) < 2 {
		for _, c := range candidates {
			if len(selectedOffers) >= 2 {
				break
			}
			alreadySelected := false
			for _, s := range selectedOffers {
				if s.OfferID == c.OfferID {
					alreadySelected = true
					break
				}
			}
			if alreadySelected {
				continue
			}
			if !c.HasSuppression {
				log.Printf("[Jarvis/Planner] WARNING: Offer %s (%s) has no suppression list — allowed for owner test accounts only", c.OfferName, c.OfferID)
			}
			selectedOffers = append(selectedOffers, c)
		}
	}

	tomorrow := now.Add(24 * time.Hour)
	sendDate := tomorrow.Format("2006-01-02")

	var plannedCampaigns []JarvisPlannedCampaign
	totalEmails := 0

	for campIdx, offer := range selectedOffers {
		var plannedRecipients []JarvisPlannedRecipient
		estimatedSends := 0

		for _, email := range ownerEmails {
			parts := strings.Split(email, "@")
			domain := ""
			if len(parts) == 2 {
				domain = parts[1]
			}
			isp := classifyISP(domain)
			optimalHours := j.getDomainOptimalHours(domain)

			stoSource := "domain"
			if len(optimalHours) == 0 {
				stoSource = "industry_default"
			}

			inboxStatus := "unknown"
			strategy := "normal"

			if isp == "Yahoo" {
				inboxStatus = "spam_suspected"
				strategy = "aggressive_inbox_recovery"
			} else if strings.Contains(email, "ignitemediagroup") {
				inboxStatus = "healthy"
				strategy = "normal"
			}

			plannedRecipients = append(plannedRecipients, JarvisPlannedRecipient{
				Email:        email,
				Domain:       domain,
				ISP:          isp,
				OptimalHours: optimalHours,
				STOSource:    stoSource,
				InboxStatus:  inboxStatus,
				Strategy:     strategy,
			})
			estimatedSends += 3
		}
		totalEmails += estimatedSends

		rationale := fmt.Sprintf(
			"Selected offer %s (ID: %s) based on 7-day Everflow performance: EPC=$%.2f, CVR=%.1f%%, %d conversions, $%.2f revenue. ",
			offer.OfferName, offer.OfferID, offer.EPC, offer.CVR, offer.Conversions, offer.Revenue,
		)
		if offer.HasSuppression {
			rationale += fmt.Sprintf("Suppression list '%s' (%s) verified with entries. ", offer.SuppressionName, offer.SuppressionID)
		} else {
			rationale += "WARNING: No matching suppression list found — allowed only because all recipients are owner test accounts. "
		}
		rationale += "Send times optimized per-inbox using domain engagement profiles."

		_ = campIdx
		plannedCampaigns = append(plannedCampaigns, JarvisPlannedCampaign{
			OfferID:           offer.OfferID,
			OfferName:         offer.OfferName,
			SuppressionListID: offer.SuppressionID,
			SuppressionName:   offer.SuppressionName,
			TrackingLink:      "",
			DurationHours:     8,
			GoalConversions:   1,
			Recipients:        plannedRecipients,
			SubjectLines:      []string{},
			Rationale:         rationale,
			EstimatedSends:    estimatedSends,
		})
	}

	strategy := JarvisStrategy{
		Objective:            "Maximize inbox placement across all owner test accounts. Focus on Yahoo inbox recovery.",
		Approach:             "STO-driven multi-wave sending with per-inbox monitoring. Yahoo receives cautious treatment with varied send times and subjects.",
		CadenceMinutes:       20,
		MaxRoundsPerCampaign: 24,
		STOEnabled:           true,
		ISPStrategies: map[string]string{
			"Gmail":     "Standard STO. Optimal hours: 10-11 AM, 7-8 PM weekend. Monitor via SparkPost events. Gmail is generally inbox-friendly.",
			"Yahoo":     "AGGRESSIVE INBOX RECOVERY. Known spam issue. Strategy: vary send times across optimal windows (8-11 AM, 6-7 PM), rotate subjects, single-send per window to avoid velocity triggers. Monitor via eDataSource inbox placement API.",
			"Microsoft": "Standard STO. Optimal hours: 10-11 AM, 7 PM weekend. Low risk.",
			"Other":     "Standard STO with industry defaults. Monitor for bounces.",
		},
		KnownIssues: []string{
			"Yahoo inbox placement is degraded — prior campaigns landed in spam for drisanjames@yahoo.com",
			"Only djames@ignitemediagroup.co consistently inboxed in previous campaigns",
			"No historical STO data exists for these specific test accounts — using domain-level defaults",
		},
		Mitigations: []string{
			"Per-recipient spam detection: if spam is detected for a specific inbox, only that inbox is paused — other Yahoo recipients unaffected",
			"eDataSource inbox placement monitoring every 10 minutes for Yahoo",
			"Subject line rotation to avoid content-based filtering",
			"Send time staggering to avoid velocity-based throttling",
			"Engagement monitoring via SparkPost Events API every 5 minutes",
		},
	}

	stoProfile := make(map[string][]int)
	for _, email := range ownerEmails {
		parts := strings.Split(email, "@")
		if len(parts) == 2 {
			stoProfile[parts[1]] = j.getDomainOptimalHours(parts[1])
		}
	}

	playbook := &JarvisPlaybook{
		PlaybookID:  uuid.New().String(),
		Name:        fmt.Sprintf("Jarvis Autonomous Plan — %s", sendDate),
		Description: fmt.Sprintf("Two-campaign inbox-focused strategy for %d owner test accounts. STO-enabled with Yahoo inbox recovery.", len(ownerEmails)),
		CreatedAt:   now,
		OfferCriteria: map[string]string{
			"selection_method":     "top_epc_7day",
			"suppression_required": "true",
			"min_clicks":           "1",
		},
		STOProfile: stoProfile,
		ISPRules: map[string]string{
			"Yahoo":     "aggressive_inbox_recovery",
			"Gmail":     "standard_sto",
			"Microsoft": "standard_sto",
			"Other":     "standard_sto",
		},
		SendCadence: "20min_retry_intervals_within_optimal_windows",
		SuccessMetrics: []string{
			"inbox_rate > 80% across all ISPs",
			"yahoo_inbox_rate > 50% (recovery target)",
			"at_least_1_conversion per campaign",
			"zero_bounces",
		},
	}

	plan := JarvisPlan{
		PlanID:        uuid.New().String(),
		CreatedAt:     now,
		Status:        "planned",
		Campaigns:     plannedCampaigns,
		Strategy:      strategy,
		OwnerAccounts: ownerEmails,
		TotalEmails:   totalEmails,
		SendDate:      sendDate,
		Playbook:      playbook,
	}

	planJSON, _ := json.Marshal(plan)
	j.db.ExecContext(ctx, `
		INSERT INTO mailing_ai_decisions (id, organization_id, decision_type, decision_reason, metrics_snapshot, ai_model, confidence, applied, created_at)
		VALUES ($1, $2, 'jarvis_autonomous_plan', $3, $4, 'jarvis-planner-v1', 0.85, false, NOW())
	`, uuid.New(), "00000000-0000-0000-0000-000000000001",
		fmt.Sprintf("Autonomous %d-campaign plan for %s with %d owner test accounts", len(plannedCampaigns), sendDate, len(ownerEmails)),
		string(planJSON))

	if len(rejectedOffers) > 0 {
		log.Printf("[Jarvis/Planner] REJECTED offers (no suppression list): %s", strings.Join(rejectedOffers, "; "))
	}

	respondJSON(w, http.StatusOK, plan)
}

func (j *JarvisOrchestrator) HandleExecutePlan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PlanID           string `json:"plan_id"`
		CampaignIndex    int    `json:"campaign_index"`
		OrganizationID   string `json:"organization_id"`
		SendingProfileID string `json:"sending_profile_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{
		"status":  "accepted",
		"message": "Use /jarvis/launch with the plan's campaign parameters to execute. The plan provides all necessary data.",
		"plan_id": req.PlanID,
	})
}
