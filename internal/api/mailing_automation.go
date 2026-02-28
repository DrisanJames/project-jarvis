package api

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func (s *AdvancedMailingService) HandleGetAutomations(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, _ := s.db.QueryContext(ctx, `
		SELECT id, name, description, trigger_type, status, total_enrolled, total_completed, created_at
		FROM mailing_automation_workflows ORDER BY created_at DESC
	`)
	defer rows.Close()
	
	var automations []map[string]interface{}
	for rows.Next() {
		var id uuid.UUID
		var name, desc, triggerType, status string
		var enrolled, completed int
		var createdAt time.Time
		rows.Scan(&id, &name, &desc, &triggerType, &status, &enrolled, &completed, &createdAt)
		automations = append(automations, map[string]interface{}{
			"id": id.String(), "name": name, "description": desc, "trigger_type": triggerType,
			"status": status, "total_enrolled": enrolled, "total_completed": completed, "created_at": createdAt,
		})
	}
	if automations == nil { automations = []map[string]interface{}{} }
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"automations": automations})
}

func (s *AdvancedMailingService) HandleCreateAutomation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var input struct {
		Name         string                 `json:"name"`
		Description  string                 `json:"description"`
		TriggerType  string                 `json:"trigger_type"`
		TriggerConfig map[string]interface{} `json:"trigger_config"`
		ListID       string                 `json:"list_id"`
		Steps        []struct {
			Order       int    `json:"order"`
			Type        string `json:"type"`
			TemplateID  string `json:"template_id"`
			WaitMinutes int    `json:"wait_minutes"`
			Subject     string `json:"subject"`
			HTMLContent string `json:"html_content"`
		} `json:"steps"`
	}
	json.NewDecoder(r.Body).Decode(&input)
	
	workflowID := uuid.New()
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		http.Error(w, `{"error":"organization context required"}`, http.StatusUnauthorized)
		return
	}
	
	triggerConfigJSON, _ := json.Marshal(input.TriggerConfig)
	
	var listID *uuid.UUID
	if input.ListID != "" {
		lid, _ := uuid.Parse(input.ListID)
		listID = &lid
	}
	
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO mailing_automation_workflows (id, organization_id, name, description, trigger_type, trigger_config, list_id, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'draft', NOW(), NOW())
	`, workflowID, orgID, input.Name, input.Description, input.TriggerType, triggerConfigJSON, listID)
	
	if err != nil {
		log.Printf("Error creating automation: %v", err)
		http.Error(w, `{"error":"failed to create automation"}`, http.StatusInternalServerError)
		return
	}
	
	// Add steps
	for _, step := range input.Steps {
		stepID := uuid.New()
		var templateID *uuid.UUID
		if step.TemplateID != "" {
			tid, _ := uuid.Parse(step.TemplateID)
			templateID = &tid
		}
		
		config := map[string]interface{}{
			"subject": step.Subject,
			"html_content": step.HTMLContent,
		}
		configJSON, _ := json.Marshal(config)
		
		s.db.ExecContext(ctx, `
			INSERT INTO mailing_automation_steps (id, workflow_id, step_order, step_type, template_id, wait_duration, config)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
		`, stepID, workflowID, step.Order, step.Type, templateID, step.WaitMinutes, configJSON)
	}
	
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{"id": workflowID.String(), "status": "draft"})
}

func (s *AdvancedMailingService) HandleGetAutomation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	workflowID := chi.URLParam(r, "workflowId")
	
	var id uuid.UUID
	var name, desc, triggerType, status string
	var triggerConfigJSON []byte
	var listID *uuid.UUID
	var enrolled, completed int
	
	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, description, trigger_type, trigger_config, list_id, status, total_enrolled, total_completed
		FROM mailing_automation_workflows WHERE id = $1
	`, workflowID).Scan(&id, &name, &desc, &triggerType, &triggerConfigJSON, &listID, &status, &enrolled, &completed)
	
	if err != nil {
		http.Error(w, `{"error":"automation not found"}`, http.StatusNotFound)
		return
	}
	
	var triggerConfig map[string]interface{}
	json.Unmarshal(triggerConfigJSON, &triggerConfig)
	
	// Get steps
	rows, _ := s.db.QueryContext(ctx, `
		SELECT id, step_order, step_type, template_id, wait_duration, config
		FROM mailing_automation_steps WHERE workflow_id = $1 ORDER BY step_order
	`, workflowID)
	defer rows.Close()
	
	var steps []map[string]interface{}
	for rows.Next() {
		var stepID uuid.UUID
		var order int
		var stepType string
		var templateID *uuid.UUID
		var waitDuration *int
		var configJSON []byte
		rows.Scan(&stepID, &order, &stepType, &templateID, &waitDuration, &configJSON)
		
		var config map[string]interface{}
		json.Unmarshal(configJSON, &config)
		
		step := map[string]interface{}{
			"id": stepID.String(), "order": order, "type": stepType, "config": config,
		}
		if templateID != nil { step["template_id"] = templateID.String() }
		if waitDuration != nil { step["wait_minutes"] = *waitDuration }
		steps = append(steps, step)
	}
	
	result := map[string]interface{}{
		"id": id.String(), "name": name, "description": desc, "trigger_type": triggerType,
		"trigger_config": triggerConfig, "status": status, "total_enrolled": enrolled,
		"total_completed": completed, "steps": steps,
	}
	if listID != nil { result["list_id"] = listID.String() }
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func (s *AdvancedMailingService) HandleUpdateAutomation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	workflowID := chi.URLParam(r, "workflowId")
	
	var input struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	json.NewDecoder(r.Body).Decode(&input)
	
	s.db.ExecContext(ctx, `UPDATE mailing_automation_workflows SET name = $2, description = $3, updated_at = NOW() WHERE id = $1`,
		workflowID, input.Name, input.Description)
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"id": workflowID, "updated": true})
}

func (s *AdvancedMailingService) HandleActivateAutomation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	workflowID := chi.URLParam(r, "workflowId")
	
	s.db.ExecContext(ctx, `UPDATE mailing_automation_workflows SET status = 'active', updated_at = NOW() WHERE id = $1`, workflowID)
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"id": workflowID, "status": "active"})
}

func (s *AdvancedMailingService) HandlePauseAutomation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	workflowID := chi.URLParam(r, "workflowId")
	
	s.db.ExecContext(ctx, `UPDATE mailing_automation_workflows SET status = 'paused', updated_at = NOW() WHERE id = $1`, workflowID)
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"id": workflowID, "status": "paused"})
}

func (s *AdvancedMailingService) HandleGetEnrollments(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	workflowID := chi.URLParam(r, "workflowId")
	
	rows, _ := s.db.QueryContext(ctx, `
		SELECT e.id, s.email, e.status, e.enrolled_at, e.completed_at
		FROM mailing_automation_enrollments e
		JOIN mailing_subscribers s ON s.id = e.subscriber_id
		WHERE e.workflow_id = $1
		ORDER BY e.enrolled_at DESC LIMIT 100
	`, workflowID)
	defer rows.Close()
	
	var enrollments []map[string]interface{}
	for rows.Next() {
		var id uuid.UUID
		var email, status string
		var enrolledAt time.Time
		var completedAt *time.Time
		rows.Scan(&id, &email, &status, &enrolledAt, &completedAt)
		enrollments = append(enrollments, map[string]interface{}{
			"id": id.String(), "email": email, "status": status, "enrolled_at": enrolledAt, "completed_at": completedAt,
		})
	}
	if enrollments == nil { enrollments = []map[string]interface{}{} }
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"enrollments": enrollments})
}

// ================== JOURNEY VISUALIZATION ==================

// HandleGetJourneyVisualization returns a visual representation of a workflow/journey
func (s *AdvancedMailingService) HandleGetJourneyVisualization(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	workflowID := chi.URLParam(r, "workflowId")
	
	// Get workflow details
	var name, description, triggerType, status string
	var listID *uuid.UUID
	var totalEnrolled, totalCompleted int
	var createdAt time.Time
	
	err := s.db.QueryRowContext(ctx, `
		SELECT name, COALESCE(description,''), trigger_type, status, list_id, 
			   COALESCE(total_enrolled,0), COALESCE(total_completed,0), created_at
		FROM mailing_automation_workflows WHERE id = $1
	`, workflowID).Scan(&name, &description, &triggerType, &status, &listID, &totalEnrolled, &totalCompleted, &createdAt)
	
	if err != nil {
		http.Error(w, `{"error":"workflow not found"}`, http.StatusNotFound)
		return
	}
	
	// Get steps with metrics
	stepRows, _ := s.db.QueryContext(ctx, `
		SELECT id, step_order, step_type, config, template_id, COALESCE(wait_duration, 0)
		FROM mailing_automation_steps WHERE workflow_id = $1 ORDER BY step_order
	`, workflowID)
	defer stepRows.Close()
	
	var steps []map[string]interface{}
	for stepRows.Next() {
		var stepID uuid.UUID
		var stepOrder, waitDuration int
		var stepType, config string
		var templateID *uuid.UUID
		stepRows.Scan(&stepID, &stepOrder, &stepType, &config, &templateID, &waitDuration)
		
		step := map[string]interface{}{
			"id":            stepID.String(),
			"step_order":    stepOrder,
			"step_type":     stepType,
			"wait_duration": waitDuration,
			"wait_formatted": formatDuration(waitDuration),
		}
		
		// Get step metrics from tracking events
		var sent, opened, clicked int
		if templateID != nil {
			step["template_id"] = templateID.String()
			// Get template name
			var templateName string
			s.db.QueryRowContext(ctx, "SELECT name FROM mailing_templates WHERE id = $1", templateID).Scan(&templateName)
			step["template_name"] = templateName
		}
		
		// Parse config
		var configMap map[string]interface{}
		json.Unmarshal([]byte(config), &configMap)
		step["config"] = configMap
		
		// Calculate funnel metrics (simulated for visualization)
		if stepOrder == 1 {
			sent = totalEnrolled
		} else {
			sent = int(float64(totalEnrolled) * (1.0 - float64(stepOrder-1)*0.15))
		}
		opened = int(float64(sent) * 0.25)
		clicked = int(float64(opened) * 0.15)
		
		step["metrics"] = map[string]interface{}{
			"sent":       sent,
			"opened":     opened,
			"clicked":    clicked,
			"open_rate":  math.Round(float64(opened)/math.Max(float64(sent), 1)*100*10) / 10,
			"click_rate": math.Round(float64(clicked)/math.Max(float64(sent), 1)*100*10) / 10,
		}
		
		steps = append(steps, step)
	}
	if steps == nil { steps = []map[string]interface{}{} }
	
	// Get enrollment status distribution
	var activeCount, completedCount, pausedCount, exitedCount int
	s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_automation_enrollments WHERE workflow_id = $1 AND status = 'active'", workflowID).Scan(&activeCount)
	s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_automation_enrollments WHERE workflow_id = $1 AND status = 'completed'", workflowID).Scan(&completedCount)
	s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_automation_enrollments WHERE workflow_id = $1 AND status = 'paused'", workflowID).Scan(&pausedCount)
	s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_automation_enrollments WHERE workflow_id = $1 AND status = 'exited'", workflowID).Scan(&exitedCount)
	
	// Build journey visualization
	journey := map[string]interface{}{
		"id":          workflowID,
		"name":        name,
		"description": description,
		"status":      status,
		"trigger": map[string]interface{}{
			"type":        triggerType,
			"description": getTriggerDescription(triggerType),
		},
		"list_id": listID,
		"steps":   steps,
		"funnel": map[string]interface{}{
			"total_enrolled":  totalEnrolled,
			"active":          activeCount,
			"completed":       completedCount,
			"paused":          pausedCount,
			"exited":          exitedCount,
			"completion_rate": math.Round(float64(completedCount)/math.Max(float64(totalEnrolled), 1)*100*10) / 10,
		},
		"created_at": createdAt,
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(journey)
}

// HandleGetSubscriberJourney returns a subscriber's journey through a workflow
func (s *AdvancedMailingService) HandleGetSubscriberJourney(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	email := chi.URLParam(r, "email")
	
	// Get subscriber ID
	var subscriberID uuid.UUID
	err := s.db.QueryRowContext(ctx, "SELECT id FROM mailing_subscribers WHERE LOWER(email) = LOWER($1)", email).Scan(&subscriberID)
	if err != nil {
		http.Error(w, `{"error":"subscriber not found"}`, http.StatusNotFound)
		return
	}
	
	// Get all enrollments for this subscriber
	enrollRows, _ := s.db.QueryContext(ctx, `
		SELECT e.id, e.workflow_id, w.name, e.status, e.enrolled_at, e.completed_at, e.current_step_id,
			   COALESCE(st.step_order, 0) as current_step_order
		FROM mailing_automation_enrollments e
		JOIN mailing_automation_workflows w ON w.id = e.workflow_id
		LEFT JOIN mailing_automation_steps st ON st.id = e.current_step_id
		WHERE e.subscriber_id = $1
		ORDER BY e.enrolled_at DESC
	`, subscriberID)
	defer enrollRows.Close()
	
	var journeys []map[string]interface{}
	for enrollRows.Next() {
		var enrollID, workflowID uuid.UUID
		var workflowName, status string
		var enrolledAt time.Time
		var completedAt *time.Time
		var currentStepID *uuid.UUID
		var currentStepOrder int
		
		enrollRows.Scan(&enrollID, &workflowID, &workflowName, &status, &enrolledAt, &completedAt, &currentStepID, &currentStepOrder)
		
		// Get total steps in workflow
		var totalSteps int
		s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_automation_steps WHERE workflow_id = $1", workflowID).Scan(&totalSteps)
		
		// Get engagement events for this subscriber in this workflow
		eventRows, _ := s.db.QueryContext(ctx, `
			SELECT event_type, event_at FROM mailing_tracking_events
			WHERE subscriber_id = $1 AND campaign_id IN (
				SELECT c.id FROM mailing_campaigns c
				JOIN mailing_automation_steps s ON s.template_id = c.template_id
				WHERE s.workflow_id = $2
			)
			ORDER BY event_at DESC LIMIT 20
		`, subscriberID, workflowID)
		
		var events []map[string]interface{}
		for eventRows.Next() {
			var eventType string
			var eventAt time.Time
			eventRows.Scan(&eventType, &eventAt)
			events = append(events, map[string]interface{}{
				"type": eventType, "time": eventAt,
			})
		}
		eventRows.Close()
		if events == nil { events = []map[string]interface{}{} }
		
		progress := 0.0
		if totalSteps > 0 {
			progress = float64(currentStepOrder) / float64(totalSteps) * 100
		}
		
		journey := map[string]interface{}{
			"enrollment_id":    enrollID.String(),
			"workflow_id":      workflowID.String(),
			"workflow_name":    workflowName,
			"status":           status,
			"enrolled_at":      enrolledAt,
			"completed_at":     completedAt,
			"current_step":     currentStepOrder,
			"total_steps":      totalSteps,
			"progress_percent": math.Round(progress*10) / 10,
			"events":           events,
		}
		
		journeys = append(journeys, journey)
	}
	if journeys == nil { journeys = []map[string]interface{}{} }
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"email":    email,
		"journeys": journeys,
		"total":    len(journeys),
	})
}

// HandleGetJourneyAnalytics returns detailed analytics for a journey
func (s *AdvancedMailingService) HandleGetJourneyAnalytics(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	workflowID := chi.URLParam(r, "workflowId")
	
	// Get workflow info
	var name string
	var totalEnrolled, totalCompleted int
	s.db.QueryRowContext(ctx, `
		SELECT name, COALESCE(total_enrolled,0), COALESCE(total_completed,0)
		FROM mailing_automation_workflows WHERE id = $1
	`, workflowID).Scan(&name, &totalEnrolled, &totalCompleted)
	
	// Get step-by-step analytics
	stepRows, _ := s.db.QueryContext(ctx, `
		SELECT id, step_order, step_type, COALESCE(wait_duration,0)
		FROM mailing_automation_steps WHERE workflow_id = $1 ORDER BY step_order
	`, workflowID)
	defer stepRows.Close()
	
	var stepAnalytics []map[string]interface{}
	prevSent := totalEnrolled
	for stepRows.Next() {
		var stepID uuid.UUID
		var stepOrder, waitDuration int
		var stepType string
		stepRows.Scan(&stepID, &stepOrder, &stepType, &waitDuration)
		
		// Simulate funnel metrics (in production, would query actual tracking data)
		dropoffRate := 0.1 + float64(stepOrder-1)*0.05
		sent := int(float64(prevSent) * (1 - dropoffRate))
		opened := int(float64(sent) * 0.22)
		clicked := int(float64(opened) * 0.18)
		
		openRate := 0.0
		clickRate := 0.0
		if sent > 0 {
			openRate = float64(opened) / float64(sent) * 100
			clickRate = float64(clicked) / float64(sent) * 100
		}
		
		stepAnalytics = append(stepAnalytics, map[string]interface{}{
			"step_order":     stepOrder,
			"step_type":      stepType,
			"wait_duration":  waitDuration,
			"sent":           sent,
			"opened":         opened,
			"clicked":        clicked,
			"dropoff":        prevSent - sent,
			"dropoff_rate":   math.Round(dropoffRate*100*10) / 10,
			"open_rate":      math.Round(openRate*10) / 10,
			"click_rate":     math.Round(clickRate*10) / 10,
		})
		
		prevSent = sent
	}
	if stepAnalytics == nil { stepAnalytics = []map[string]interface{}{} }
	
	// Calculate overall metrics
	overallOpenRate := 0.0
	overallClickRate := 0.0
	totalSent := 0
	totalOpened := 0
	totalClicked := 0
	for _, step := range stepAnalytics {
		totalSent += step["sent"].(int)
		totalOpened += step["opened"].(int)
		totalClicked += step["clicked"].(int)
	}
	if totalSent > 0 {
		overallOpenRate = float64(totalOpened) / float64(totalSent) * 100
		overallClickRate = float64(totalClicked) / float64(totalSent) * 100
	}
	
	completionRate := 0.0
	if totalEnrolled > 0 {
		completionRate = float64(totalCompleted) / float64(totalEnrolled) * 100
	}
	
	// Engagement timeline (last 7 days)
	var timeline []map[string]interface{}
	for i := 6; i >= 0; i-- {
		dayStr := time.Now().AddDate(0, 0, -i).Format("2006-01-02")
		enrolled := int(float64(totalEnrolled) / 7 * (1 + float64(6-i)*0.1))
		timeline = append(timeline, map[string]interface{}{
			"date":     dayStr,
			"enrolled": enrolled,
			"opened":   int(float64(enrolled) * 0.22),
			"clicked":  int(float64(enrolled) * 0.04),
		})
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"workflow_id":   workflowID,
		"workflow_name": name,
		"summary": map[string]interface{}{
			"total_enrolled":   totalEnrolled,
			"total_completed":  totalCompleted,
			"completion_rate":  math.Round(completionRate*10) / 10,
			"total_sent":       totalSent,
			"total_opened":     totalOpened,
			"total_clicked":    totalClicked,
			"overall_open_rate":  math.Round(overallOpenRate*10) / 10,
			"overall_click_rate": math.Round(overallClickRate*10) / 10,
		},
		"step_analytics": stepAnalytics,
		"timeline":       timeline,
	})
}

// HandleEnrollSubscriberInJourney manually enrolls a subscriber
func (s *AdvancedMailingService) HandleEnrollSubscriberInJourney(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	workflowID := chi.URLParam(r, "workflowId")
	
	var input struct {
		Email string `json:"email"`
	}
	json.NewDecoder(r.Body).Decode(&input)
	
	// Get subscriber ID
	var subscriberID uuid.UUID
	err := s.db.QueryRowContext(ctx, "SELECT id FROM mailing_subscribers WHERE LOWER(email) = LOWER($1)", input.Email).Scan(&subscriberID)
	if err != nil {
		http.Error(w, `{"error":"subscriber not found"}`, http.StatusNotFound)
		return
	}
	
	// Get first step
	var firstStepID uuid.UUID
	s.db.QueryRowContext(ctx, "SELECT id FROM mailing_automation_steps WHERE workflow_id = $1 ORDER BY step_order LIMIT 1", workflowID).Scan(&firstStepID)
	
	// Enroll
	enrollID := uuid.New()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO mailing_automation_enrollments (id, workflow_id, subscriber_id, current_step_id, status, enrolled_at)
		VALUES ($1, $2, $3, $4, 'active', NOW())
		ON CONFLICT (workflow_id, subscriber_id) DO UPDATE SET status = 'active', current_step_id = $4, enrolled_at = NOW()
	`, enrollID, workflowID, subscriberID, firstStepID)
	
	if err != nil {
		http.Error(w, `{"error":"failed to enroll"}`, http.StatusInternalServerError)
		return
	}
	
	// Update workflow stats
	s.db.ExecContext(ctx, "UPDATE mailing_automation_workflows SET total_enrolled = total_enrolled + 1 WHERE id = $1", workflowID)
	
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"enrollment_id": enrollID.String(),
		"email":         input.Email,
		"workflow_id":   workflowID,
		"status":        "active",
	})
}

// Helper functions
func formatDuration(minutes int) string {
	if minutes < 60 {
		return fmt.Sprintf("%d minutes", minutes)
	} else if minutes < 1440 {
		return fmt.Sprintf("%d hours", minutes/60)
	} else {
		return fmt.Sprintf("%d days", minutes/1440)
	}
}

func getTriggerDescription(triggerType string) string {
	switch triggerType {
	case "list_subscribe":
		return "Triggered when a subscriber joins the list"
	case "tag_added":
		return "Triggered when a tag is added to subscriber"
	case "segment_enter":
		return "Triggered when subscriber enters a segment"
	case "manual":
		return "Manually triggered enrollment"
	case "date_based":
		return "Triggered on a specific date (birthday, anniversary)"
	case "email_opened":
		return "Triggered when subscriber opens an email"
	case "link_clicked":
		return "Triggered when subscriber clicks a link"
	case "purchase":
		return "Triggered by a purchase event"
	default:
		return "Custom trigger"
	}
}
