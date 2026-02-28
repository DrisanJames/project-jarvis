package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func (s *AdvancedMailingService) HandleGetABTests(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, _ := s.db.QueryContext(ctx, `
		SELECT t.id, t.campaign_id, c.name, t.test_type, t.sample_size_percent, 
			   t.winner_criteria, t.status, t.created_at
		FROM mailing_ab_tests t
		JOIN mailing_campaigns c ON c.id = t.campaign_id
		ORDER BY t.created_at DESC LIMIT 50
	`)
	defer rows.Close()
	
	var tests []map[string]interface{}
	for rows.Next() {
		var id, campaignID uuid.UUID
		var name, testType, winnerCriteria, status string
		var sampleSize int
		var createdAt time.Time
		rows.Scan(&id, &campaignID, &name, &testType, &sampleSize, &winnerCriteria, &status, &createdAt)
		tests = append(tests, map[string]interface{}{
			"id": id.String(), "campaign_id": campaignID.String(), "campaign_name": name,
			"test_type": testType, "sample_size_percent": sampleSize,
			"winner_criteria": winnerCriteria, "status": status, "created_at": createdAt,
		})
	}
	if tests == nil { tests = []map[string]interface{}{} }
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"tests": tests})
}

func (s *AdvancedMailingService) HandleCreateABTest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var input struct {
		CampaignID        string `json:"campaign_id"`
		TestType          string `json:"test_type"`
		SampleSizePercent int    `json:"sample_size_percent"`
		WinnerCriteria    string `json:"winner_criteria"`
		WinnerWaitHours   int    `json:"winner_wait_hours"`
		Variants          []struct {
			Name        string `json:"name"`
			Subject     string `json:"subject"`
			FromName    string `json:"from_name"`
			HTMLContent string `json:"html_content"`
		} `json:"variants"`
	}
	json.NewDecoder(r.Body).Decode(&input)
	
	testID := uuid.New()
	campaignID, _ := uuid.Parse(input.CampaignID)
	
	if input.SampleSizePercent == 0 { input.SampleSizePercent = 20 }
	if input.WinnerCriteria == "" { input.WinnerCriteria = "open_rate" }
	if input.WinnerWaitHours == 0 { input.WinnerWaitHours = 4 }
	
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO mailing_ab_tests (id, campaign_id, test_type, sample_size_percent, winner_criteria, winner_wait_hours, status)
		VALUES ($1, $2, $3, $4, $5, $6, 'draft')
	`, testID, campaignID, input.TestType, input.SampleSizePercent, input.WinnerCriteria, input.WinnerWaitHours)
	
	if err != nil {
		http.Error(w, `{"error":"failed to create test"}`, http.StatusInternalServerError)
		return
	}
	
	// Create variants
	for _, v := range input.Variants {
		s.db.ExecContext(ctx, `
			INSERT INTO mailing_ab_variants (id, test_id, variant_name, subject, from_name, html_content)
			VALUES ($1, $2, $3, $4, $5, $6)
		`, uuid.New(), testID, v.Name, v.Subject, v.FromName, v.HTMLContent)
	}
	
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id": testID.String(), "status": "draft",
	})
}

func (s *AdvancedMailingService) HandleGetABTest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	testID := chi.URLParam(r, "testId")
	
	var id, campaignID uuid.UUID
	var testType, winnerCriteria, status string
	var sampleSize, waitHours int
	var winnerVariantID *uuid.UUID
	
	err := s.db.QueryRowContext(ctx, `
		SELECT id, campaign_id, test_type, sample_size_percent, winner_criteria, winner_wait_hours, status, winner_variant_id
		FROM mailing_ab_tests WHERE id = $1
	`, testID).Scan(&id, &campaignID, &testType, &sampleSize, &winnerCriteria, &waitHours, &status, &winnerVariantID)
	
	if err != nil {
		http.Error(w, `{"error":"test not found"}`, http.StatusNotFound)
		return
	}
	
	// Get variants
	rows, _ := s.db.QueryContext(ctx, `
		SELECT id, variant_name, subject, from_name, sent_count, open_count, click_count, revenue, is_winner
		FROM mailing_ab_variants WHERE test_id = $1 ORDER BY variant_name
	`, testID)
	defer rows.Close()
	
	var variants []map[string]interface{}
	for rows.Next() {
		var vid uuid.UUID
		var name, subject, fromName string
		var sent, opens, clicks int
		var revenue float64
		var isWinner bool
		rows.Scan(&vid, &name, &subject, &fromName, &sent, &opens, &clicks, &revenue, &isWinner)
		
		openRate := 0.0
		clickRate := 0.0
		if sent > 0 {
			openRate = float64(opens) / float64(sent) * 100
			clickRate = float64(clicks) / float64(sent) * 100
		}
		
		variants = append(variants, map[string]interface{}{
			"id": vid.String(), "name": name, "subject": subject, "from_name": fromName,
			"sent_count": sent, "open_count": opens, "click_count": clicks, "revenue": revenue,
			"open_rate": openRate, "click_rate": clickRate, "is_winner": isWinner,
		})
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id": id.String(), "campaign_id": campaignID.String(), "test_type": testType,
		"sample_size_percent": sampleSize, "winner_criteria": winnerCriteria,
		"winner_wait_hours": waitHours, "status": status, "variants": variants,
	})
}

func (s *AdvancedMailingService) HandleStartABTest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	testID := chi.URLParam(r, "testId")
	
	s.db.ExecContext(ctx, `UPDATE mailing_ab_tests SET status = 'testing' WHERE id = $1`, testID)
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"status": "testing"})
}

func (s *AdvancedMailingService) HandlePickWinner(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	testID := chi.URLParam(r, "testId")
	
	var input struct {
		VariantID string `json:"variant_id"`
	}
	json.NewDecoder(r.Body).Decode(&input)
	
	// Auto-pick if no variant specified
	if input.VariantID == "" {
		var criteria string
		s.db.QueryRowContext(ctx, `SELECT winner_criteria FROM mailing_ab_tests WHERE id = $1`, testID).Scan(&criteria)
		
		orderBy := "open_count DESC"
		if criteria == "click_rate" {
			orderBy = "click_count DESC"
		} else if criteria == "revenue" {
			orderBy = "revenue DESC"
		}
		
		s.db.QueryRowContext(ctx, fmt.Sprintf(`
			SELECT id FROM mailing_ab_variants WHERE test_id = $1 ORDER BY %s LIMIT 1
		`, orderBy), testID).Scan(&input.VariantID)
	}
	
	// Mark winner
	s.db.ExecContext(ctx, `UPDATE mailing_ab_variants SET is_winner = false WHERE test_id = $1`, testID)
	s.db.ExecContext(ctx, `UPDATE mailing_ab_variants SET is_winner = true WHERE id = $1`, input.VariantID)
	s.db.ExecContext(ctx, `UPDATE mailing_ab_tests SET status = 'winner_selected', winner_variant_id = $2, completed_at = NOW() WHERE id = $1`, testID, input.VariantID)
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"winner_variant_id": input.VariantID})
}
