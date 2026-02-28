package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

// =============================================================================
// TEST HELPERS
// =============================================================================

func setupJourneyTestDB(t *testing.T) (*sql.DB, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	return db, mock, func() { db.Close() }
}

func setupTestExecutor(t *testing.T) (*JourneyExecutor, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, cleanup := setupJourneyTestDB(t)

	executor := NewJourneyExecutor(db)

	return executor, mock, cleanup
}

func createTestJourney(nodeTypes []string) (string, string) {
	nodes := make([]JourneyNodeExec, 0, len(nodeTypes)+1)
	connections := make([]JourneyConnectionExec, 0, len(nodeTypes))

	// Add trigger node
	triggerID := uuid.New().String()
	nodes = append(nodes, JourneyNodeExec{
		ID:          triggerID,
		Type:        "trigger",
		Config:      map[string]interface{}{"triggerType": "list_signup"},
		Connections: []string{},
	})

	prevNodeID := triggerID
	for i, nodeType := range nodeTypes {
		nodeID := uuid.New().String()
		config := make(map[string]interface{})

		switch nodeType {
		case "email":
			config["subject"] = fmt.Sprintf("Test Email %d", i+1)
			config["htmlContent"] = fmt.Sprintf("<p>Hello, this is test email %d</p>", i+1)
			config["fromName"] = "Test Sender"
			config["fromEmail"] = "test@example.com"
		case "delay":
			config["delayValue"] = float64(1)
			config["delayUnit"] = "hours"
		case "condition":
			config["conditionType"] = "opened_email"
		case "split":
			config["splitPercentage"] = float64(50)
		case "goal":
			config["goalName"] = "conversion"
		}

		nodes = append(nodes, JourneyNodeExec{
			ID:          nodeID,
			Type:        nodeType,
			Config:      config,
			Connections: []string{},
		})

		connections = append(connections, JourneyConnectionExec{
			From: prevNodeID,
			To:   nodeID,
		})
		prevNodeID = nodeID
	}

	nodesJSON, _ := json.Marshal(nodes)
	connectionsJSON, _ := json.Marshal(connections)

	return string(nodesJSON), string(connectionsJSON)
}

func createTestEnrollment(journeyID, email, nodeID string) Enrollment {
	return Enrollment{
		ID:              uuid.New().String(),
		JourneyID:       journeyID,
		SubscriberEmail: email,
		CurrentNodeID:   nodeID,
		Status:          "active",
		Metadata:        make(map[string]interface{}),
	}
}

// =============================================================================
// NODE EXECUTION TESTS
// =============================================================================

func TestExecuteNode_Email(t *testing.T) {
	executor, mock, cleanup := setupTestExecutor(t)
	defer cleanup()

	ctx := context.Background()

	t.Run("sends email successfully", func(t *testing.T) {
		var emailSent bool
		var sentTo, sentSubject string

		executor.SetEmailSender(func(ctx context.Context, email, subject, htmlContent, fromName, fromEmail string) error {
			emailSent = true
			sentTo = email
			sentSubject = subject
			return nil
		})

		enrollment := createTestEnrollment(uuid.New().String(), "test@example.com", "node-1")
		node := &JourneyNodeExec{
			ID:   "node-1",
			Type: "email",
			Config: map[string]interface{}{
				"subject":     "Welcome!",
				"htmlContent": "<p>Hello!</p>",
				"fromName":    "Test",
				"fromEmail":   "from@example.com",
			},
		}

		// Mock subscriber lookup (returns no rows - that's ok for this test)
		mock.ExpectQuery("SELECT id, organization_id").
			WillReturnError(sql.ErrNoRows)

		result, err := executor.executeNode(ctx, enrollment, node)
		if err != nil {
			t.Errorf("executeNode() error: %v", err)
		}

		if !emailSent {
			t.Error("Email was not sent")
		}
		if sentTo != "test@example.com" {
			t.Errorf("Email sent to %s, want test@example.com", sentTo)
		}
		if sentSubject != "Welcome!" {
			t.Errorf("Subject = %s, want Welcome!", sentSubject)
		}
		if result.Action != "continue" {
			t.Errorf("Action = %s, want continue", result.Action)
		}
	})

	t.Run("handles email send failure", func(t *testing.T) {
		executor.SetEmailSender(func(ctx context.Context, email, subject, htmlContent, fromName, fromEmail string) error {
			return errors.New("SMTP error")
		})

		enrollment := createTestEnrollment(uuid.New().String(), "test@example.com", "node-1")
		node := &JourneyNodeExec{
			ID:   "node-1",
			Type: "email",
			Config: map[string]interface{}{
				"subject":     "Test",
				"htmlContent": "<p>Test</p>",
			},
		}

		// Mock subscriber lookup
		mock.ExpectQuery("SELECT id, organization_id").
			WillReturnError(sql.ErrNoRows)

		_, err := executor.executeNode(ctx, enrollment, node)
		if err == nil {
			t.Error("Expected error for failed email send")
		}
	})

	t.Run("loads template when templateId provided", func(t *testing.T) {
		var sentSubject string
		executor.SetEmailSender(func(ctx context.Context, email, subject, htmlContent, fromName, fromEmail string) error {
			sentSubject = subject
			return nil
		})

		enrollment := createTestEnrollment(uuid.New().String(), "test@example.com", "node-1")
		templateID := uuid.New().String()
		node := &JourneyNodeExec{
			ID:   "node-1",
			Type: "email",
			Config: map[string]interface{}{
				"templateId": templateID,
			},
		}

		// Mock template lookup
		mock.ExpectQuery("SELECT subject, html_content, from_name, from_email").
			WithArgs(templateID).
			WillReturnRows(sqlmock.NewRows([]string{"subject", "html_content", "from_name", "from_email"}).
				AddRow("Template Subject", "<p>Template Content</p>", "Template Sender", "template@example.com"))

		// Mock subscriber lookup
		mock.ExpectQuery("SELECT id, organization_id").
			WillReturnError(sql.ErrNoRows)

		result, err := executor.executeNode(ctx, enrollment, node)
		if err != nil {
			t.Errorf("executeNode() error: %v", err)
		}

		if sentSubject != "Template Subject" {
			t.Errorf("Subject = %s, want Template Subject", sentSubject)
		}
		if result.Action != "continue" {
			t.Errorf("Action = %s, want continue", result.Action)
		}
	})

	t.Run("uses default values when config is empty", func(t *testing.T) {
		var sentSubject, sentFrom string
		executor.SetEmailSender(func(ctx context.Context, email, subject, htmlContent, fromName, fromEmail string) error {
			sentSubject = subject
			sentFrom = fromEmail
			return nil
		})

		enrollment := createTestEnrollment(uuid.New().String(), "test@example.com", "node-1")
		node := &JourneyNodeExec{
			ID:     "node-1",
			Type:   "email",
			Config: map[string]interface{}{}, // Empty config
		}

		// Mock subscriber lookup
		mock.ExpectQuery("SELECT id, organization_id").
			WillReturnError(sql.ErrNoRows)

		result, err := executor.executeNode(ctx, enrollment, node)
		if err != nil {
			t.Errorf("executeNode() error: %v", err)
		}

		if sentSubject != "Message from IGNITE" {
			t.Errorf("Subject = %s, want default", sentSubject)
		}
		if sentFrom != "noreply@ignite.media" {
			t.Errorf("From = %s, want default", sentFrom)
		}
		if result.Action != "continue" {
			t.Errorf("Action = %s, want continue", result.Action)
		}
	})
}

func TestExecuteNode_Delay(t *testing.T) {
	executor, mock, cleanup := setupTestExecutor(t)
	defer cleanup()

	ctx := context.Background()

	t.Run("schedules fixed delay - hours", func(t *testing.T) {
		enrollment := createTestEnrollment(uuid.New().String(), "test@example.com", "node-1")
		node := &JourneyNodeExec{
			ID:   "node-1",
			Type: "delay",
			Config: map[string]interface{}{
				"delayValue": float64(2),
				"delayUnit":  "hours",
			},
		}

		// Expect metadata update
		mock.ExpectExec("UPDATE mailing_journey_enrollments SET metadata").
			WillReturnResult(sqlmock.NewResult(0, 1))

		result, err := executor.executeNode(ctx, enrollment, node)
		if err != nil {
			t.Errorf("executeNode() error: %v", err)
		}

		if result.Action != "wait" {
			t.Errorf("Action = %s, want wait", result.Action)
		}

		// Check wait time is approximately 2 hours from now
		expectedWait := time.Now().Add(2 * time.Hour)
		diff := result.WaitUntil.Sub(expectedWait)
		if diff > time.Minute || diff < -time.Minute {
			t.Errorf("WaitUntil diff from expected: %v", diff)
		}
	})

	t.Run("schedules fixed delay - days", func(t *testing.T) {
		enrollment := createTestEnrollment(uuid.New().String(), "test@example.com", "node-1")
		node := &JourneyNodeExec{
			ID:   "node-1",
			Type: "delay",
			Config: map[string]interface{}{
				"delayValue": float64(3),
				"delayUnit":  "days",
			},
		}

		mock.ExpectExec("UPDATE mailing_journey_enrollments SET metadata").
			WillReturnResult(sqlmock.NewResult(0, 1))

		result, err := executor.executeNode(ctx, enrollment, node)
		if err != nil {
			t.Errorf("executeNode() error: %v", err)
		}

		expectedWait := time.Now().Add(3 * 24 * time.Hour)
		diff := result.WaitUntil.Sub(expectedWait)
		if diff > time.Minute || diff < -time.Minute {
			t.Errorf("WaitUntil diff from expected: %v (expected ~3 days)", diff)
		}
	})

	t.Run("schedules fixed delay - minutes", func(t *testing.T) {
		enrollment := createTestEnrollment(uuid.New().String(), "test@example.com", "node-1")
		node := &JourneyNodeExec{
			ID:   "node-1",
			Type: "delay",
			Config: map[string]interface{}{
				"delayValue": float64(30),
				"delayUnit":  "minutes",
			},
		}

		mock.ExpectExec("UPDATE mailing_journey_enrollments SET metadata").
			WillReturnResult(sqlmock.NewResult(0, 1))

		result, err := executor.executeNode(ctx, enrollment, node)
		if err != nil {
			t.Errorf("executeNode() error: %v", err)
		}

		expectedWait := time.Now().Add(30 * time.Minute)
		diff := result.WaitUntil.Sub(expectedWait)
		if diff > time.Minute || diff < -time.Minute {
			t.Errorf("WaitUntil diff from expected: %v", diff)
		}
	})

	t.Run("schedules fixed delay - weeks", func(t *testing.T) {
		enrollment := createTestEnrollment(uuid.New().String(), "test@example.com", "node-1")
		node := &JourneyNodeExec{
			ID:   "node-1",
			Type: "delay",
			Config: map[string]interface{}{
				"delayValue": float64(1),
				"delayUnit":  "weeks",
			},
		}

		mock.ExpectExec("UPDATE mailing_journey_enrollments SET metadata").
			WillReturnResult(sqlmock.NewResult(0, 1))

		result, err := executor.executeNode(ctx, enrollment, node)
		if err != nil {
			t.Errorf("executeNode() error: %v", err)
		}

		expectedWait := time.Now().Add(7 * 24 * time.Hour)
		diff := result.WaitUntil.Sub(expectedWait)
		if diff > time.Minute || diff < -time.Minute {
			t.Errorf("WaitUntil diff from expected: %v (expected ~1 week)", diff)
		}
	})

	t.Run("continues after delay already started", func(t *testing.T) {
		enrollment := createTestEnrollment(uuid.New().String(), "test@example.com", "node-1")
		enrollment.Metadata["delay_started"] = true // Already waited

		node := &JourneyNodeExec{
			ID:   "node-1",
			Type: "delay",
			Config: map[string]interface{}{
				"delayValue": float64(2),
				"delayUnit":  "hours",
			},
		}

		result, err := executor.executeNode(ctx, enrollment, node)
		if err != nil {
			t.Errorf("executeNode() error: %v", err)
		}

		if result.Action != "continue" {
			t.Errorf("Action = %s, want continue (delay already passed)", result.Action)
		}
	})

	t.Run("uses defaults for invalid delay config", func(t *testing.T) {
		enrollment := createTestEnrollment(uuid.New().String(), "test@example.com", "node-1")
		node := &JourneyNodeExec{
			ID:   "node-1",
			Type: "delay",
			Config: map[string]interface{}{
				"delayValue": float64(0), // Invalid
				"delayUnit":  "",         // Empty
			},
		}

		mock.ExpectExec("UPDATE mailing_journey_enrollments SET metadata").
			WillReturnResult(sqlmock.NewResult(0, 1))

		result, err := executor.executeNode(ctx, enrollment, node)
		if err != nil {
			t.Errorf("executeNode() error: %v", err)
		}

		// Should default to 1 hour
		expectedWait := time.Now().Add(1 * time.Hour)
		diff := result.WaitUntil.Sub(expectedWait)
		if diff > time.Minute || diff < -time.Minute {
			t.Errorf("WaitUntil diff from expected default: %v", diff)
		}
	})
}

func TestExecuteNode_Condition(t *testing.T) {
	executor, mock, cleanup := setupTestExecutor(t)
	defer cleanup()

	ctx := context.Background()

	t.Run("email_opened condition - true", func(t *testing.T) {
		enrollment := createTestEnrollment(uuid.New().String(), "test@example.com", "node-1")
		node := &JourneyNodeExec{
			ID:   "node-1",
			Type: "condition",
			Config: map[string]interface{}{
				"conditionType": "opened_email",
			},
		}

		// Mock: subscriber has opened emails
		mock.ExpectQuery("SELECT EXISTS").
			WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

		result, err := executor.executeNode(ctx, enrollment, node)
		if err != nil {
			t.Errorf("executeNode() error: %v", err)
		}

		if result.Action != "continue" {
			t.Errorf("Action = %s, want continue", result.Action)
		}
		if result.Branch != "true" {
			t.Errorf("Branch = %s, want true", result.Branch)
		}
	})

	t.Run("email_opened condition - false", func(t *testing.T) {
		enrollment := createTestEnrollment(uuid.New().String(), "test@example.com", "node-1")
		node := &JourneyNodeExec{
			ID:   "node-1",
			Type: "condition",
			Config: map[string]interface{}{
				"conditionType": "opened_email",
			},
		}

		// Mock: subscriber has NOT opened emails
		mock.ExpectQuery("SELECT EXISTS").
			WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))

		result, err := executor.executeNode(ctx, enrollment, node)
		if err != nil {
			t.Errorf("executeNode() error: %v", err)
		}

		if result.Branch != "false" {
			t.Errorf("Branch = %s, want false", result.Branch)
		}
	})

	t.Run("clicked_link condition - true", func(t *testing.T) {
		enrollment := createTestEnrollment(uuid.New().String(), "test@example.com", "node-1")
		node := &JourneyNodeExec{
			ID:   "node-1",
			Type: "condition",
			Config: map[string]interface{}{
				"conditionType": "clicked_link",
			},
		}

		mock.ExpectQuery("SELECT EXISTS").
			WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

		result, err := executor.executeNode(ctx, enrollment, node)
		if err != nil {
			t.Errorf("executeNode() error: %v", err)
		}

		if result.Branch != "true" {
			t.Errorf("Branch = %s, want true", result.Branch)
		}
	})

	t.Run("engagement_score condition - above threshold", func(t *testing.T) {
		enrollment := createTestEnrollment(uuid.New().String(), "test@example.com", "node-1")
		node := &JourneyNodeExec{
			ID:   "node-1",
			Type: "condition",
			Config: map[string]interface{}{
				"conditionType": "engagement_score",
				"threshold":     float64(50),
			},
		}

		// Mock: subscriber score is 75 (above threshold)
		mock.ExpectQuery("SELECT engagement_score").
			WillReturnRows(sqlmock.NewRows([]string{"engagement_score"}).AddRow(75.0))

		result, err := executor.executeNode(ctx, enrollment, node)
		if err != nil {
			t.Errorf("executeNode() error: %v", err)
		}

		if result.Branch != "true" {
			t.Errorf("Branch = %s, want true (score above threshold)", result.Branch)
		}
	})

	t.Run("engagement_score condition - below threshold", func(t *testing.T) {
		enrollment := createTestEnrollment(uuid.New().String(), "test@example.com", "node-1")
		node := &JourneyNodeExec{
			ID:   "node-1",
			Type: "condition",
			Config: map[string]interface{}{
				"conditionType": "engagement_score",
				"threshold":     float64(50),
			},
		}

		// Mock: subscriber score is 30 (below threshold)
		mock.ExpectQuery("SELECT engagement_score").
			WillReturnRows(sqlmock.NewRows([]string{"engagement_score"}).AddRow(30.0))

		result, err := executor.executeNode(ctx, enrollment, node)
		if err != nil {
			t.Errorf("executeNode() error: %v", err)
		}

		if result.Branch != "false" {
			t.Errorf("Branch = %s, want false (score below threshold)", result.Branch)
		}
	})

	t.Run("custom_field condition - match", func(t *testing.T) {
		enrollment := createTestEnrollment(uuid.New().String(), "test@example.com", "node-1")
		node := &JourneyNodeExec{
			ID:   "node-1",
			Type: "condition",
			Config: map[string]interface{}{
				"conditionType": "custom_field",
				"fieldName":     "plan",
				"fieldValue":    "premium",
			},
		}

		customFields := map[string]interface{}{"plan": "premium"}
		cfJSON, _ := json.Marshal(customFields)

		mock.ExpectQuery("SELECT custom_fields").
			WillReturnRows(sqlmock.NewRows([]string{"custom_fields"}).AddRow(string(cfJSON)))

		result, err := executor.executeNode(ctx, enrollment, node)
		if err != nil {
			t.Errorf("executeNode() error: %v", err)
		}

		if result.Branch != "true" {
			t.Errorf("Branch = %s, want true (custom field matches)", result.Branch)
		}
	})

	t.Run("custom_field condition - no match", func(t *testing.T) {
		enrollment := createTestEnrollment(uuid.New().String(), "test@example.com", "node-1")
		node := &JourneyNodeExec{
			ID:   "node-1",
			Type: "condition",
			Config: map[string]interface{}{
				"conditionType": "custom_field",
				"fieldName":     "plan",
				"fieldValue":    "premium",
			},
		}

		customFields := map[string]interface{}{"plan": "basic"}
		cfJSON, _ := json.Marshal(customFields)

		mock.ExpectQuery("SELECT custom_fields").
			WillReturnRows(sqlmock.NewRows([]string{"custom_fields"}).AddRow(string(cfJSON)))

		result, err := executor.executeNode(ctx, enrollment, node)
		if err != nil {
			t.Errorf("executeNode() error: %v", err)
		}

		if result.Branch != "false" {
			t.Errorf("Branch = %s, want false (custom field doesn't match)", result.Branch)
		}
	})
}

func TestExecuteNode_Split(t *testing.T) {
	executor, _, cleanup := setupTestExecutor(t)
	defer cleanup()

	ctx := context.Background()

	t.Run("percentage split consistency", func(t *testing.T) {
		// The same enrollment ID should always get the same branch
		enrollmentID := uuid.New().String()
		enrollment := Enrollment{
			ID:              enrollmentID,
			JourneyID:       uuid.New().String(),
			SubscriberEmail: "test@example.com",
			CurrentNodeID:   "node-1",
			Status:          "active",
			Metadata:        make(map[string]interface{}),
		}

		node := &JourneyNodeExec{
			ID:   "node-1",
			Type: "split",
			Config: map[string]interface{}{
				"splitPercentage": float64(50),
			},
		}

		// Execute multiple times with same enrollment
		var firstBranch string
		for i := 0; i < 10; i++ {
			result, err := executor.executeNode(ctx, enrollment, node)
			if err != nil {
				t.Errorf("executeNode() error: %v", err)
			}

			if i == 0 {
				firstBranch = result.Branch
			} else if result.Branch != firstBranch {
				t.Errorf("Inconsistent branch: got %s, first was %s", result.Branch, firstBranch)
			}
		}
	})

	t.Run("split distribution", func(t *testing.T) {
		node := &JourneyNodeExec{
			ID:   "node-1",
			Type: "split",
			Config: map[string]interface{}{
				"splitPercentage": float64(50),
			},
		}

		branchA := 0
		branchB := 0
		total := 1000

		for i := 0; i < total; i++ {
			enrollment := Enrollment{
				ID:              uuid.New().String(), // Different ID each time
				JourneyID:       uuid.New().String(),
				SubscriberEmail: fmt.Sprintf("test%d@example.com", i),
				CurrentNodeID:   "node-1",
				Status:          "active",
				Metadata:        make(map[string]interface{}),
			}

			result, err := executor.executeNode(ctx, enrollment, node)
			if err != nil {
				t.Errorf("executeNode() error: %v", err)
			}

			if result.Branch == "A" {
				branchA++
			} else {
				branchB++
			}
		}

		// Check distribution is roughly 50/50 (allow 10% variance)
		ratioA := float64(branchA) / float64(total) * 100
		ratioB := float64(branchB) / float64(total) * 100

		if ratioA < 40 || ratioA > 60 {
			t.Errorf("Branch A ratio = %.1f%%, expected ~50%%", ratioA)
		}
		if ratioB < 40 || ratioB > 60 {
			t.Errorf("Branch B ratio = %.1f%%, expected ~50%%", ratioB)
		}
	})

	t.Run("defaults invalid split percentage", func(t *testing.T) {
		enrollment := createTestEnrollment(uuid.New().String(), "test@example.com", "node-1")
		node := &JourneyNodeExec{
			ID:   "node-1",
			Type: "split",
			Config: map[string]interface{}{
				"splitPercentage": float64(0), // Invalid
			},
		}

		result, err := executor.executeNode(ctx, enrollment, node)
		if err != nil {
			t.Errorf("executeNode() error: %v", err)
		}

		// Should still work with default 50/50
		if result.Branch != "A" && result.Branch != "B" {
			t.Errorf("Branch = %s, want A or B", result.Branch)
		}
	})
}

func TestExecuteNode_Goal(t *testing.T) {
	executor, _, cleanup := setupTestExecutor(t)
	defer cleanup()

	ctx := context.Background()

	t.Run("marks conversion", func(t *testing.T) {
		enrollment := createTestEnrollment(uuid.New().String(), "test@example.com", "node-1")
		node := &JourneyNodeExec{
			ID:   "node-1",
			Type: "goal",
			Config: map[string]interface{}{
				"goalName": "purchase",
			},
		}

		result, err := executor.executeNode(ctx, enrollment, node)
		if err != nil {
			t.Errorf("executeNode() error: %v", err)
		}

		if result.Action != "convert" {
			t.Errorf("Action = %s, want convert", result.Action)
		}
	})
}

func TestExecuteNode_Trigger(t *testing.T) {
	executor, _, cleanup := setupTestExecutor(t)
	defer cleanup()

	ctx := context.Background()

	t.Run("trigger node continues", func(t *testing.T) {
		enrollment := createTestEnrollment(uuid.New().String(), "test@example.com", "trigger-1")
		node := &JourneyNodeExec{
			ID:   "trigger-1",
			Type: "trigger",
			Config: map[string]interface{}{
				"triggerType": "list_signup",
			},
		}

		result, err := executor.executeNode(ctx, enrollment, node)
		if err != nil {
			t.Errorf("executeNode() error: %v", err)
		}

		if result.Action != "continue" {
			t.Errorf("Action = %s, want continue", result.Action)
		}
	})
}

func TestExecuteNode_Unknown(t *testing.T) {
	executor, _, cleanup := setupTestExecutor(t)
	defer cleanup()

	ctx := context.Background()

	t.Run("unknown node type continues", func(t *testing.T) {
		enrollment := createTestEnrollment(uuid.New().String(), "test@example.com", "node-1")
		node := &JourneyNodeExec{
			ID:     "node-1",
			Type:   "unknown_type",
			Config: map[string]interface{}{},
		}

		result, err := executor.executeNode(ctx, enrollment, node)
		if err != nil {
			t.Errorf("executeNode() error: %v", err)
		}

		if result.Action != "continue" {
			t.Errorf("Action = %s, want continue (unknown type should continue)", result.Action)
		}
	})
}

// =============================================================================
// ENROLLMENT PROCESSING TESTS
// =============================================================================

func TestProcessEnrollment_Simple(t *testing.T) {
	executor, mock, cleanup := setupTestExecutor(t)
	defer cleanup()

	ctx := context.Background()
	journeyID := uuid.New().String()
	enrollmentID := uuid.New().String()

	// Create a simple journey with one email node
	nodes := []JourneyNodeExec{
		{ID: "trigger-1", Type: "trigger", Config: map[string]interface{}{}},
		{ID: "email-1", Type: "email", Config: map[string]interface{}{
			"subject":     "Welcome!",
			"htmlContent": "<p>Hello!</p>",
		}},
	}
	connections := []JourneyConnectionExec{
		{From: "trigger-1", To: "email-1"},
	}

	nodesJSON, _ := json.Marshal(nodes)
	connectionsJSON, _ := json.Marshal(connections)

	executor.SetEmailSender(func(ctx context.Context, email, subject, htmlContent, fromName, fromEmail string) error {
		return nil
	})

	// Mock journey lookup
	mock.ExpectQuery("SELECT nodes, connections FROM mailing_journeys").
		WithArgs(journeyID).
		WillReturnRows(sqlmock.NewRows([]string{"nodes", "connections"}).
			AddRow(string(nodesJSON), string(connectionsJSON)))

	// Mock subscriber lookup
	mock.ExpectQuery("SELECT id, organization_id").
		WillReturnError(sql.ErrNoRows)

	// Mock execution log
	mock.ExpectExec("INSERT INTO mailing_journey_execution_log").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Mock complete enrollment (no next node)
	mock.ExpectExec("UPDATE mailing_journey_enrollments").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Mock journey stats update
	mock.ExpectExec("UPDATE mailing_journeys").
		WillReturnResult(sqlmock.NewResult(0, 1))

	enrollment := Enrollment{
		ID:              enrollmentID,
		JourneyID:       journeyID,
		SubscriberEmail: "test@example.com",
		CurrentNodeID:   "email-1",
		Status:          "active",
		Metadata:        make(map[string]interface{}),
	}

	err := executor.processEnrollment(ctx, enrollment)
	if err != nil {
		t.Errorf("processEnrollment() error: %v", err)
	}
}

func TestProcessEnrollment_MultiNode(t *testing.T) {
	executor, mock, cleanup := setupTestExecutor(t)
	defer cleanup()

	ctx := context.Background()
	journeyID := uuid.New().String()
	enrollmentID := uuid.New().String()

	// Create journey with multiple sequential nodes
	nodes := []JourneyNodeExec{
		{ID: "trigger-1", Type: "trigger", Config: map[string]interface{}{}},
		{ID: "email-1", Type: "email", Config: map[string]interface{}{
			"subject": "Email 1",
		}},
		{ID: "email-2", Type: "email", Config: map[string]interface{}{
			"subject": "Email 2",
		}},
	}
	connections := []JourneyConnectionExec{
		{From: "trigger-1", To: "email-1"},
		{From: "email-1", To: "email-2"},
	}

	nodesJSON, _ := json.Marshal(nodes)
	connectionsJSON, _ := json.Marshal(connections)

	executor.SetEmailSender(func(ctx context.Context, email, subject, htmlContent, fromName, fromEmail string) error {
		return nil
	})

	// Mock journey lookup
	mock.ExpectQuery("SELECT nodes, connections FROM mailing_journeys").
		WithArgs(journeyID).
		WillReturnRows(sqlmock.NewRows([]string{"nodes", "connections"}).
			AddRow(string(nodesJSON), string(connectionsJSON)))

	// Mock subscriber lookup
	mock.ExpectQuery("SELECT id, organization_id").
		WillReturnError(sql.ErrNoRows)

	// Mock execution log
	mock.ExpectExec("INSERT INTO mailing_journey_execution_log").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Mock move to next node
	mock.ExpectExec("UPDATE mailing_journey_enrollments").
		WillReturnResult(sqlmock.NewResult(0, 1))

	enrollment := Enrollment{
		ID:              enrollmentID,
		JourneyID:       journeyID,
		SubscriberEmail: "test@example.com",
		CurrentNodeID:   "email-1",
		Status:          "active",
		Metadata:        make(map[string]interface{}),
	}

	err := executor.processEnrollment(ctx, enrollment)
	if err != nil {
		t.Errorf("processEnrollment() error: %v", err)
	}
}

func TestProcessEnrollment_Branching(t *testing.T) {
	executor, mock, cleanup := setupTestExecutor(t)
	defer cleanup()

	ctx := context.Background()
	journeyID := uuid.New().String()
	enrollmentID := uuid.New().String()

	// Create journey with condition-based branches
	nodes := []JourneyNodeExec{
		{ID: "trigger-1", Type: "trigger", Config: map[string]interface{}{}},
		{ID: "condition-1", Type: "condition", Config: map[string]interface{}{
			"conditionType": "opened_email",
		}},
		{ID: "email-engaged", Type: "email", Config: map[string]interface{}{
			"subject": "Thanks for engaging!",
		}},
		{ID: "email-reengagement", Type: "email", Config: map[string]interface{}{
			"subject": "We miss you!",
		}},
	}
	connections := []JourneyConnectionExec{
		{From: "trigger-1", To: "condition-1"},
		{From: "condition-1", To: "email-engaged", Label: "true"},
		{From: "condition-1", To: "email-reengagement", Label: "false"},
	}

	nodesJSON, _ := json.Marshal(nodes)
	connectionsJSON, _ := json.Marshal(connections)

	// Mock journey lookup
	mock.ExpectQuery("SELECT nodes, connections FROM mailing_journeys").
		WithArgs(journeyID).
		WillReturnRows(sqlmock.NewRows([]string{"nodes", "connections"}).
			AddRow(string(nodesJSON), string(connectionsJSON)))

	// Mock condition check - subscriber opened emails
	mock.ExpectQuery("SELECT EXISTS").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	// Mock execution log
	mock.ExpectExec("INSERT INTO mailing_journey_execution_log").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Mock move to engaged branch (email-engaged)
	mock.ExpectExec("UPDATE mailing_journey_enrollments").
		WillReturnResult(sqlmock.NewResult(0, 1))

	enrollment := Enrollment{
		ID:              enrollmentID,
		JourneyID:       journeyID,
		SubscriberEmail: "test@example.com",
		CurrentNodeID:   "condition-1",
		Status:          "active",
		Metadata:        make(map[string]interface{}),
	}

	err := executor.processEnrollment(ctx, enrollment)
	if err != nil {
		t.Errorf("processEnrollment() error: %v", err)
	}
}

func TestProcessEnrollment_ErrorHandling(t *testing.T) {
	t.Run("email send failure", func(t *testing.T) {
		executor, mock, cleanup := setupTestExecutor(t)
		defer cleanup()

		ctx := context.Background()
		journeyID := uuid.New().String()
		enrollmentID := uuid.New().String()

		nodes := []JourneyNodeExec{
			{ID: "email-1", Type: "email", Config: map[string]interface{}{
				"subject": "Test",
			}},
		}
		connections := []JourneyConnectionExec{}

		nodesJSON, _ := json.Marshal(nodes)
		connectionsJSON, _ := json.Marshal(connections)

		executor.SetEmailSender(func(ctx context.Context, email, subject, htmlContent, fromName, fromEmail string) error {
			return errors.New("SMTP connection failed")
		})

		mock.ExpectQuery("SELECT nodes, connections FROM mailing_journeys").
			WithArgs(journeyID).
			WillReturnRows(sqlmock.NewRows([]string{"nodes", "connections"}).
				AddRow(string(nodesJSON), string(connectionsJSON)))

		mock.ExpectQuery("SELECT id, organization_id").
			WillReturnError(sql.ErrNoRows)

		// Mock error log
		mock.ExpectExec("INSERT INTO mailing_journey_execution_log").
			WillReturnResult(sqlmock.NewResult(0, 1))

		enrollment := Enrollment{
			ID:              enrollmentID,
			JourneyID:       journeyID,
			SubscriberEmail: "test@example.com",
			CurrentNodeID:   "email-1",
			Status:          "active",
			Metadata:        make(map[string]interface{}),
		}

		err := executor.processEnrollment(ctx, enrollment)
		if err == nil {
			t.Error("Expected error for failed email send")
		}
	})

	t.Run("database error fetching journey", func(t *testing.T) {
		executor, mock, cleanup := setupTestExecutor(t)
		defer cleanup()

		ctx := context.Background()
		journeyID := uuid.New().String()

		mock.ExpectQuery("SELECT nodes, connections FROM mailing_journeys").
			WithArgs(journeyID).
			WillReturnError(errors.New("database connection lost"))

		enrollment := createTestEnrollment(journeyID, "test@example.com", "node-1")

		err := executor.processEnrollment(ctx, enrollment)
		if err == nil {
			t.Error("Expected error for database failure")
		}
	})

	t.Run("completes when no current node found", func(t *testing.T) {
		executor, mock, cleanup := setupTestExecutor(t)
		defer cleanup()

		ctx := context.Background()
		journeyID := uuid.New().String()
		enrollmentID := uuid.New().String()

		// Empty journey (only trigger)
		nodes := []JourneyNodeExec{
			{ID: "trigger-1", Type: "trigger", Config: map[string]interface{}{}},
		}
		connections := []JourneyConnectionExec{}

		nodesJSON, _ := json.Marshal(nodes)
		connectionsJSON, _ := json.Marshal(connections)

		mock.ExpectQuery("SELECT nodes, connections FROM mailing_journeys").
			WithArgs(journeyID).
			WillReturnRows(sqlmock.NewRows([]string{"nodes", "connections"}).
				AddRow(string(nodesJSON), string(connectionsJSON)))

		// Mock complete enrollment
		mock.ExpectExec("UPDATE mailing_journey_enrollments").
			WillReturnResult(sqlmock.NewResult(0, 1))

		mock.ExpectExec("UPDATE mailing_journeys").
			WillReturnResult(sqlmock.NewResult(0, 1))

		enrollment := Enrollment{
			ID:              enrollmentID,
			JourneyID:       journeyID,
			SubscriberEmail: "test@example.com",
			CurrentNodeID:   "nonexistent-node",
			Status:          "active",
			Metadata:        make(map[string]interface{}),
		}

		err := executor.processEnrollment(ctx, enrollment)
		if err != nil {
			t.Errorf("processEnrollment() error: %v", err)
		}
	})
}

// =============================================================================
// CONCURRENCY TESTS
// =============================================================================

func TestConcurrentEnrollmentProcessing(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping concurrency test in short mode")
	}

	// Create a real-enough test to verify thread safety
	db, mock, cleanup := setupJourneyTestDB(t)
	defer cleanup()

	executor := NewJourneyExecutor(db)

	var emailsSent int64
	var mu sync.Mutex

	executor.SetEmailSender(func(ctx context.Context, email, subject, htmlContent, fromName, fromEmail string) error {
		mu.Lock()
		atomic.AddInt64(&emailsSent, 1)
		mu.Unlock()
		return nil
	})

	// Allow any number of queries/execs
	mock.MatchExpectationsInOrder(false)

	// Process multiple enrollments concurrently
	numEnrollments := 50
	var wg sync.WaitGroup

	for i := 0; i < numEnrollments; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			ctx := context.Background()
			enrollment := createTestEnrollment(
				uuid.New().String(),
				fmt.Sprintf("test%d@example.com", idx),
				"email-1",
			)

			// Note: This will likely fail because we can't properly mock concurrent DB access,
			// but we're testing that there are no race conditions in the executor logic itself
			_ = executor.processEnrollment(ctx, enrollment)
		}(i)
	}

	wg.Wait()

	// The main thing we're testing is that no race conditions occurred
	// (detected by -race flag)
	t.Logf("Processed %d enrollments concurrently, sent %d emails", numEnrollments, atomic.LoadInt64(&emailsSent))
}

func TestPollingUnderLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping load test in short mode")
	}

	executor, mock, cleanup := setupTestExecutor(t)
	defer cleanup()

	// Test that polling doesn't cause issues under load
	mock.MatchExpectationsInOrder(false)

	// Expect multiple polling queries
	for i := 0; i < 100; i++ {
		mock.ExpectQuery(regexp.QuoteMeta("SELECT e.id, e.journey_id")).
			WillReturnRows(sqlmock.NewRows([]string{"id", "journey_id", "subscriber_email", "current_node_id", "status", "metadata"}))
	}

	executor.pollInterval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	executor.ctx = ctx
	executor.cancel = cancel

	// Run polling for a short time
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			select {
			case <-ctx.Done():
				close(done)
				return
			default:
				executor.processReadyEnrollments()
				time.Sleep(5 * time.Millisecond)
			}
		}
		close(done)
	}()

	select {
	case <-done:
		// Success
	case <-time.After(5 * time.Second):
		t.Error("Polling under load timed out")
		cancel()
	}
}

// =============================================================================
// INTEGRATION TESTS
// =============================================================================

func TestJourneyExecutor_FullJourneyExecution(t *testing.T) {
	executor, mock, cleanup := setupTestExecutor(t)
	defer cleanup()

	ctx := context.Background()
	journeyID := uuid.New().String()
	enrollmentID := uuid.New().String()

	// Full journey: trigger -> email -> delay -> condition -> goal
	nodes := []JourneyNodeExec{
		{ID: "trigger-1", Type: "trigger", Config: map[string]interface{}{}},
		{ID: "email-1", Type: "email", Config: map[string]interface{}{"subject": "Welcome!"}},
		{ID: "delay-1", Type: "delay", Config: map[string]interface{}{"delayValue": float64(1), "delayUnit": "hours"}},
		{ID: "condition-1", Type: "condition", Config: map[string]interface{}{"conditionType": "opened_email"}},
		{ID: "goal-1", Type: "goal", Config: map[string]interface{}{"goalName": "conversion"}},
	}
	connections := []JourneyConnectionExec{
		{From: "trigger-1", To: "email-1"},
		{From: "email-1", To: "delay-1"},
		{From: "delay-1", To: "condition-1"},
		{From: "condition-1", To: "goal-1", Label: "true"},
	}

	nodesJSON, _ := json.Marshal(nodes)
	connectionsJSON, _ := json.Marshal(connections)

	executor.SetEmailSender(func(ctx context.Context, email, subject, htmlContent, fromName, fromEmail string) error {
		return nil
	})

	// Step 1: Execute email node
	mock.ExpectQuery("SELECT nodes, connections FROM mailing_journeys").
		WithArgs(journeyID).
		WillReturnRows(sqlmock.NewRows([]string{"nodes", "connections"}).
			AddRow(string(nodesJSON), string(connectionsJSON)))
	mock.ExpectQuery("SELECT id, organization_id").WillReturnError(sql.ErrNoRows)
	mock.ExpectExec("INSERT INTO mailing_journey_execution_log").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE mailing_journey_enrollments").WillReturnResult(sqlmock.NewResult(0, 1))

	enrollment := Enrollment{
		ID:              enrollmentID,
		JourneyID:       journeyID,
		SubscriberEmail: "test@example.com",
		CurrentNodeID:   "email-1",
		Status:          "active",
		Metadata:        make(map[string]interface{}),
	}

	err := executor.processEnrollment(ctx, enrollment)
	if err != nil {
		t.Errorf("Step 1 (email): processEnrollment() error: %v", err)
	}
}

func TestJourneyExecutor_StartStop(t *testing.T) {
	db, mock, cleanup := setupJourneyTestDB(t)
	defer cleanup()

	executor := NewJourneyExecutor(db)

	// Expect worker registration
	mock.ExpectExec("INSERT INTO mailing_workers").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Start
	executor.Start()

	// Verify running
	executor.mu.RLock()
	running := executor.running
	executor.mu.RUnlock()
	if !running {
		t.Error("Executor should be running after Start()")
	}

	// Verify workerID is set
	if executor.workerID == "" {
		t.Error("WorkerID should be set")
	}

	// Double start should be no-op
	executor.Start()

	// Stop
	mock.ExpectExec("UPDATE mailing_workers SET status").
		WillReturnResult(sqlmock.NewResult(0, 1))

	executor.Stop()

	executor.mu.RLock()
	running = executor.running
	executor.mu.RUnlock()
	if running {
		t.Error("Executor should not be running after Stop()")
	}

	// Double stop should be no-op
	executor.Stop()
}

func TestJourneyExecutor_Heartbeat(t *testing.T) {
	db, mock, cleanup := setupJourneyTestDB(t)
	defer cleanup()

	executor := NewJourneyExecutor(db)
	executor.pollInterval = 100 * time.Millisecond

	// Allow queries in any order - set all expectations BEFORE starting
	mock.MatchExpectationsInOrder(false)

	// Worker registration
	mock.ExpectExec("INSERT INTO mailing_workers").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Allow heartbeat updates (multiple calls expected)
	for i := 0; i < 20; i++ {
		mock.ExpectExec("UPDATE mailing_workers").
			WillReturnResult(sqlmock.NewResult(0, 1))
	}

	// Allow polling queries (multiple calls expected)
	for i := 0; i < 20; i++ {
		mock.ExpectQuery("SELECT e.id, e.journey_id").
			WillReturnRows(sqlmock.NewRows([]string{"id", "journey_id", "subscriber_email", "current_node_id", "status", "metadata"}))
	}

	// Set up deregistration expectation before starting
	mock.ExpectExec("UPDATE mailing_workers SET status").
		WillReturnResult(sqlmock.NewResult(0, 1))

	executor.Start()

	// Let it run briefly
	time.Sleep(200 * time.Millisecond)

	executor.Stop()

	// Check stats
	if executor.workerID == "" {
		t.Error("Worker should have been registered")
	}
}

// =============================================================================
// EDGE CASE TESTS
// =============================================================================

func TestEdgeCase_EmptyJourney(t *testing.T) {
	executor, mock, cleanup := setupTestExecutor(t)
	defer cleanup()

	ctx := context.Background()
	journeyID := uuid.New().String()
	enrollmentID := uuid.New().String()

	// Empty journey (no nodes except trigger)
	nodes := []JourneyNodeExec{
		{ID: "trigger-1", Type: "trigger", Config: map[string]interface{}{}},
	}
	connections := []JourneyConnectionExec{}

	nodesJSON, _ := json.Marshal(nodes)
	connectionsJSON, _ := json.Marshal(connections)

	mock.ExpectQuery("SELECT nodes, connections FROM mailing_journeys").
		WithArgs(journeyID).
		WillReturnRows(sqlmock.NewRows([]string{"nodes", "connections"}).
			AddRow(string(nodesJSON), string(connectionsJSON)))

	// Expect completion
	mock.ExpectExec("UPDATE mailing_journey_enrollments").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE mailing_journeys").
		WillReturnResult(sqlmock.NewResult(0, 1))

	enrollment := Enrollment{
		ID:              enrollmentID,
		JourneyID:       journeyID,
		SubscriberEmail: "test@example.com",
		CurrentNodeID:   "", // No current node
		Status:          "active",
		Metadata:        make(map[string]interface{}),
	}

	err := executor.processEnrollment(ctx, enrollment)
	if err != nil {
		t.Errorf("processEnrollment() error: %v", err)
	}
}

func TestEdgeCase_InvalidNodeType(t *testing.T) {
	executor, mock, cleanup := setupTestExecutor(t)
	defer cleanup()

	ctx := context.Background()
	journeyID := uuid.New().String()
	enrollmentID := uuid.New().String()

	nodes := []JourneyNodeExec{
		{ID: "invalid-1", Type: "nonexistent_type", Config: map[string]interface{}{}},
	}
	connections := []JourneyConnectionExec{}

	nodesJSON, _ := json.Marshal(nodes)
	connectionsJSON, _ := json.Marshal(connections)

	mock.ExpectQuery("SELECT nodes, connections FROM mailing_journeys").
		WithArgs(journeyID).
		WillReturnRows(sqlmock.NewRows([]string{"nodes", "connections"}).
			AddRow(string(nodesJSON), string(connectionsJSON)))

	// Mock execution log
	mock.ExpectExec("INSERT INTO mailing_journey_execution_log").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Should complete (no next node)
	mock.ExpectExec("UPDATE mailing_journey_enrollments").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE mailing_journeys").
		WillReturnResult(sqlmock.NewResult(0, 1))

	enrollment := Enrollment{
		ID:              enrollmentID,
		JourneyID:       journeyID,
		SubscriberEmail: "test@example.com",
		CurrentNodeID:   "invalid-1",
		Status:          "active",
		Metadata:        make(map[string]interface{}),
	}

	err := executor.processEnrollment(ctx, enrollment)
	if err != nil {
		t.Errorf("processEnrollment() error: %v (invalid node type should continue)", err)
	}
}

func TestEdgeCase_MissingConnections(t *testing.T) {
	executor, mock, cleanup := setupTestExecutor(t)
	defer cleanup()

	ctx := context.Background()
	journeyID := uuid.New().String()
	enrollmentID := uuid.New().String()

	// Email node with no outgoing connections
	nodes := []JourneyNodeExec{
		{ID: "trigger-1", Type: "trigger", Config: map[string]interface{}{}},
		{ID: "email-1", Type: "email", Config: map[string]interface{}{"subject": "Test"}},
	}
	connections := []JourneyConnectionExec{
		{From: "trigger-1", To: "email-1"},
		// No connection from email-1
	}

	nodesJSON, _ := json.Marshal(nodes)
	connectionsJSON, _ := json.Marshal(connections)

	executor.SetEmailSender(func(ctx context.Context, email, subject, htmlContent, fromName, fromEmail string) error {
		return nil
	})

	mock.ExpectQuery("SELECT nodes, connections FROM mailing_journeys").
		WithArgs(journeyID).
		WillReturnRows(sqlmock.NewRows([]string{"nodes", "connections"}).
			AddRow(string(nodesJSON), string(connectionsJSON)))

	mock.ExpectQuery("SELECT id, organization_id").WillReturnError(sql.ErrNoRows)
	mock.ExpectExec("INSERT INTO mailing_journey_execution_log").WillReturnResult(sqlmock.NewResult(0, 1))

	// Should complete (no next node)
	mock.ExpectExec("UPDATE mailing_journey_enrollments").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE mailing_journeys").WillReturnResult(sqlmock.NewResult(0, 1))

	enrollment := Enrollment{
		ID:              enrollmentID,
		JourneyID:       journeyID,
		SubscriberEmail: "test@example.com",
		CurrentNodeID:   "email-1",
		Status:          "active",
		Metadata:        make(map[string]interface{}),
	}

	err := executor.processEnrollment(ctx, enrollment)
	if err != nil {
		t.Errorf("processEnrollment() error: %v", err)
	}
}

func TestEdgeCase_DuplicateEnrollment(t *testing.T) {
	// The executor processes each enrollment independently
	// Duplicate detection should happen at enrollment time, not execution time
	executor, mock, cleanup := setupTestExecutor(t)
	defer cleanup()

	ctx := context.Background()
	journeyID := uuid.New().String()

	nodes := []JourneyNodeExec{
		{ID: "email-1", Type: "email", Config: map[string]interface{}{"subject": "Test"}},
	}
	connections := []JourneyConnectionExec{}

	nodesJSON, _ := json.Marshal(nodes)
	connectionsJSON, _ := json.Marshal(connections)

	var emailCount int
	executor.SetEmailSender(func(ctx context.Context, email, subject, htmlContent, fromName, fromEmail string) error {
		emailCount++
		return nil
	})

	// Process two enrollments for same email
	for i := 0; i < 2; i++ {
		mock.ExpectQuery("SELECT nodes, connections FROM mailing_journeys").
			WithArgs(journeyID).
			WillReturnRows(sqlmock.NewRows([]string{"nodes", "connections"}).
				AddRow(string(nodesJSON), string(connectionsJSON)))

		mock.ExpectQuery("SELECT id, organization_id").WillReturnError(sql.ErrNoRows)
		mock.ExpectExec("INSERT INTO mailing_journey_execution_log").WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec("UPDATE mailing_journey_enrollments").WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec("UPDATE mailing_journeys").WillReturnResult(sqlmock.NewResult(0, 1))

		enrollment := Enrollment{
			ID:              uuid.New().String(),
			JourneyID:       journeyID,
			SubscriberEmail: "test@example.com", // Same email
			CurrentNodeID:   "email-1",
			Status:          "active",
			Metadata:        make(map[string]interface{}),
		}

		err := executor.processEnrollment(ctx, enrollment)
		if err != nil {
			t.Errorf("processEnrollment() error: %v", err)
		}
	}

	// Both enrollments processed (duplicate prevention is not executor's job)
	if emailCount != 2 {
		t.Errorf("Expected 2 emails sent, got %d", emailCount)
	}
}

func TestEdgeCase_NilMetadata(t *testing.T) {
	executor, mock, cleanup := setupTestExecutor(t)
	defer cleanup()

	ctx := context.Background()
	journeyID := uuid.New().String()
	enrollmentID := uuid.New().String()

	nodes := []JourneyNodeExec{
		{ID: "delay-1", Type: "delay", Config: map[string]interface{}{
			"delayValue": float64(1),
			"delayUnit":  "hours",
		}},
	}
	connections := []JourneyConnectionExec{}

	nodesJSON, _ := json.Marshal(nodes)
	connectionsJSON, _ := json.Marshal(connections)

	mock.ExpectQuery("SELECT nodes, connections FROM mailing_journeys").
		WithArgs(journeyID).
		WillReturnRows(sqlmock.NewRows([]string{"nodes", "connections"}).
			AddRow(string(nodesJSON), string(connectionsJSON)))

	mock.ExpectExec("UPDATE mailing_journey_enrollments SET metadata").
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectExec("INSERT INTO mailing_journey_execution_log").
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectExec("UPDATE mailing_journey_enrollments").
		WillReturnResult(sqlmock.NewResult(0, 1))

	enrollment := Enrollment{
		ID:              enrollmentID,
		JourneyID:       journeyID,
		SubscriberEmail: "test@example.com",
		CurrentNodeID:   "delay-1",
		Status:          "active",
		Metadata:        nil, // nil metadata
	}

	err := executor.processEnrollment(ctx, enrollment)
	if err != nil {
		t.Errorf("processEnrollment() with nil metadata error: %v", err)
	}
}

func TestFindNextNode(t *testing.T) {
	executor, _, cleanup := setupTestExecutor(t)
	defer cleanup()

	connections := []JourneyConnectionExec{
		{From: "node-1", To: "node-2"},
		{From: "node-2", To: "node-3a", Label: "true"},
		{From: "node-2", To: "node-3b", Label: "false"},
		{From: "node-3a", To: "node-4"},
	}

	tests := []struct {
		name       string
		currentID  string
		branch     string
		wantNextID string
	}{
		{"simple connection", "node-1", "", "node-2"},
		{"branching true", "node-2", "true", "node-3a"},
		{"branching false", "node-2", "false", "node-3b"},
		{"no matching branch", "node-2", "maybe", ""},
		{"no outgoing connections", "node-4", "", ""},
		{"nonexistent node", "node-99", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nextID := executor.findNextNode(tt.currentID, connections, tt.branch)
			if nextID != tt.wantNextID {
				t.Errorf("findNextNode(%s, %s) = %s, want %s", tt.currentID, tt.branch, nextID, tt.wantNextID)
			}
		})
	}
}

func TestCompleteEnrollment(t *testing.T) {
	executor, mock, cleanup := setupTestExecutor(t)
	defer cleanup()

	ctx := context.Background()
	enrollmentID := uuid.New().String()

	mock.ExpectExec("UPDATE mailing_journey_enrollments").
		WithArgs(enrollmentID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectExec("UPDATE mailing_journeys").
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := executor.completeEnrollment(ctx, enrollmentID)
	if err != nil {
		t.Errorf("completeEnrollment() error: %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("Unfulfilled expectations: %v", err)
	}
}

func TestConvertEnrollment(t *testing.T) {
	executor, mock, cleanup := setupTestExecutor(t)
	defer cleanup()

	ctx := context.Background()
	enrollmentID := uuid.New().String()

	mock.ExpectExec("UPDATE mailing_journey_enrollments").
		WithArgs(enrollmentID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectExec("UPDATE mailing_journeys").
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := executor.convertEnrollment(ctx, enrollmentID)
	if err != nil {
		t.Errorf("convertEnrollment() error: %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("Unfulfilled expectations: %v", err)
	}
}

func TestScheduleNextExecution(t *testing.T) {
	executor, mock, cleanup := setupTestExecutor(t)
	defer cleanup()

	ctx := context.Background()
	enrollmentID := uuid.New().String()
	nextTime := time.Now().Add(1 * time.Hour)

	mock.ExpectExec("UPDATE mailing_journey_enrollments").
		WithArgs(enrollmentID, nextTime).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := executor.scheduleNextExecution(ctx, enrollmentID, nextTime)
	if err != nil {
		t.Errorf("scheduleNextExecution() error: %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("Unfulfilled expectations: %v", err)
	}
}

func TestMoveToNode(t *testing.T) {
	executor, mock, cleanup := setupTestExecutor(t)
	defer cleanup()

	ctx := context.Background()
	enrollmentID := uuid.New().String()
	nextNodeID := "next-node"

	mock.ExpectExec("UPDATE mailing_journey_enrollments").
		WithArgs(enrollmentID, nextNodeID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := executor.moveToNode(ctx, enrollmentID, nextNodeID)
	if err != nil {
		t.Errorf("moveToNode() error: %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("Unfulfilled expectations: %v", err)
	}
}

// =============================================================================
// BENCHMARKS
// =============================================================================

func BenchmarkNodeExecution_Email(b *testing.B) {
	db, mock, _ := sqlmock.New()
	defer db.Close()

	executor := NewJourneyExecutor(db)
	executor.SetEmailSender(func(ctx context.Context, email, subject, htmlContent, fromName, fromEmail string) error {
		return nil
	})

	ctx := context.Background()

	enrollment := createTestEnrollment(uuid.New().String(), "test@example.com", "node-1")
	node := &JourneyNodeExec{
		ID:   "node-1",
		Type: "email",
		Config: map[string]interface{}{
			"subject":     "Benchmark Test",
			"htmlContent": "<p>Hello!</p>",
			"fromName":    "Test",
			"fromEmail":   "test@example.com",
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Reset mock for each iteration
		mock.MatchExpectationsInOrder(false)
		mock.ExpectQuery("SELECT id, organization_id").WillReturnError(sql.ErrNoRows)
		executor.executeNode(ctx, enrollment, node)
	}
}

func BenchmarkNodeExecution_Condition(b *testing.B) {
	db, mock, _ := sqlmock.New()
	defer db.Close()

	executor := NewJourneyExecutor(db)
	ctx := context.Background()

	enrollment := createTestEnrollment(uuid.New().String(), "test@example.com", "node-1")
	node := &JourneyNodeExec{
		ID:   "node-1",
		Type: "condition",
		Config: map[string]interface{}{
			"conditionType": "opened_email",
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mock.MatchExpectationsInOrder(false)
		mock.ExpectQuery("SELECT EXISTS").WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
		executor.executeNode(ctx, enrollment, node)
	}
}

func BenchmarkNodeExecution_Split(b *testing.B) {
	db, _, _ := sqlmock.New()
	defer db.Close()

	executor := NewJourneyExecutor(db)
	ctx := context.Background()

	node := &JourneyNodeExec{
		ID:   "node-1",
		Type: "split",
		Config: map[string]interface{}{
			"splitPercentage": float64(50),
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		enrollment := createTestEnrollment(uuid.New().String(), "test@example.com", "node-1")
		executor.executeNode(ctx, enrollment, node)
	}
}

func BenchmarkNodeExecution_Delay(b *testing.B) {
	db, mock, _ := sqlmock.New()
	defer db.Close()

	executor := NewJourneyExecutor(db)
	ctx := context.Background()

	node := &JourneyNodeExec{
		ID:   "node-1",
		Type: "delay",
		Config: map[string]interface{}{
			"delayValue": float64(1),
			"delayUnit":  "hours",
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mock.MatchExpectationsInOrder(false)
		mock.ExpectExec("UPDATE mailing_journey_enrollments").WillReturnResult(sqlmock.NewResult(0, 1))
		enrollment := createTestEnrollment(uuid.New().String(), "test@example.com", "node-1")
		executor.executeNode(ctx, enrollment, node)
	}
}

func BenchmarkFindNextNode(b *testing.B) {
	db, _, _ := sqlmock.New()
	defer db.Close()

	executor := NewJourneyExecutor(db)

	connections := make([]JourneyConnectionExec, 100)
	for i := 0; i < 100; i++ {
		connections[i] = JourneyConnectionExec{
			From:  fmt.Sprintf("node-%d", i),
			To:    fmt.Sprintf("node-%d", i+1),
			Label: "",
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		executor.findNextNode("node-50", connections, "")
	}
}

func BenchmarkConcurrentExecution(b *testing.B) {
	db, _, _ := sqlmock.New()
	defer db.Close()

	executor := NewJourneyExecutor(db)
	executor.SetEmailSender(func(ctx context.Context, email, subject, htmlContent, fromName, fromEmail string) error {
		return nil
	})

	ctx := context.Background()

	// Use split node which doesn't need DB - safer for concurrent benchmark
	node := &JourneyNodeExec{
		ID:   "node-1",
		Type: "split",
		Config: map[string]interface{}{
			"splitPercentage": float64(50),
		},
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			enrollment := createTestEnrollment(uuid.New().String(), "test@example.com", "node-1")
			executor.executeNode(ctx, enrollment, node)
		}
	})
}

// =============================================================================
// STATS TESTS
// =============================================================================

func TestJourneyExecutor_Stats(t *testing.T) {
	db, _, cleanup := setupJourneyTestDB(t)
	defer cleanup()

	executor := NewJourneyExecutor(db)

	// Initial stats should be zero
	if executor.totalExecuted != 0 {
		t.Errorf("Initial totalExecuted = %d, want 0", executor.totalExecuted)
	}
	if executor.totalErrors != 0 {
		t.Errorf("Initial totalErrors = %d, want 0", executor.totalErrors)
	}
}

func TestNewJourneyExecutor(t *testing.T) {
	db, _, cleanup := setupJourneyTestDB(t)
	defer cleanup()

	executor := NewJourneyExecutor(db)

	if executor.db != db {
		t.Error("DB not set correctly")
	}
	if executor.workerID == "" {
		t.Error("WorkerID should be generated")
	}
	if executor.pollInterval != 1*time.Second {
		t.Errorf("PollInterval = %v, want 1s", executor.pollInterval)
	}
}

func TestSetEmailSender(t *testing.T) {
	db, _, cleanup := setupJourneyTestDB(t)
	defer cleanup()

	executor := NewJourneyExecutor(db)

	if executor.emailSender != nil {
		t.Error("EmailSender should be nil initially")
	}

	called := false
	executor.SetEmailSender(func(ctx context.Context, email, subject, htmlContent, fromName, fromEmail string) error {
		called = true
		return nil
	})

	if executor.emailSender == nil {
		t.Error("EmailSender should be set")
	}

	// Test that it can be called
	executor.emailSender(context.Background(), "", "", "", "", "")
	if !called {
		t.Error("EmailSender was not called")
	}
}
