package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/mailing"
)

// JourneyExecutor handles executing journey workflows
type JourneyExecutor struct {
	db           *sql.DB
	workerID     string
	pollInterval time.Duration
	
	// Stats
	totalExecuted int64
	totalErrors   int64
	
	// Control
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	running bool
	mu      sync.RWMutex
	
	// Email sender (injected)
	emailSender func(ctx context.Context, email, subject, htmlContent, fromName, fromEmail string) error
}

// JourneyNode represents a node in a journey
type JourneyNodeExec struct {
	ID          string                 `json:"id"`
	Type        string                 `json:"type"` // trigger, email, delay, condition, split, goal
	Config      map[string]interface{} `json:"config"`
	Connections []string               `json:"connections"`
}

// JourneyConnection represents a connection in a journey
type JourneyConnectionExec struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Label string `json:"label,omitempty"`
}

// Enrollment represents a subscriber enrolled in a journey
type Enrollment struct {
	ID             string
	JourneyID      string
	SubscriberEmail string
	CurrentNodeID  string
	Status         string
	NextExecuteAt  sql.NullTime
	Metadata       map[string]interface{}
}

// NewJourneyExecutor creates a new journey executor
func NewJourneyExecutor(db *sql.DB) *JourneyExecutor {
	return &JourneyExecutor{
		db:           db,
		workerID:     fmt.Sprintf("journey-%s", uuid.New().String()[:8]),
		pollInterval: 1 * time.Second,
	}
}

// SetEmailSender sets the email sending function
func (je *JourneyExecutor) SetEmailSender(sender func(ctx context.Context, email, subject, htmlContent, fromName, fromEmail string) error) {
	je.emailSender = sender
}

// Start begins the journey executor
func (je *JourneyExecutor) Start() {
	je.mu.Lock()
	if je.running {
		je.mu.Unlock()
		return
	}
	je.running = true
	je.ctx, je.cancel = context.WithCancel(context.Background())
	je.mu.Unlock()
	
	log.Printf("JourneyExecutor: Starting worker %s", je.workerID)
	
	// Register worker
	je.registerWorker()
	
	// Start main loop
	je.wg.Add(1)
	go je.executionLoop()
	
	// Start heartbeat (also tracked in WaitGroup)
	je.wg.Add(1)
	go je.heartbeatLoop()
}

// Stop gracefully stops the executor with a timeout
func (je *JourneyExecutor) Stop() {
	je.mu.Lock()
	if !je.running {
		je.mu.Unlock()
		return
	}
	je.running = false
	je.cancel()
	je.mu.Unlock()
	
	log.Println("JourneyExecutor: Stopping...")
	
	// Wait for goroutines with timeout
	done := make(chan struct{})
	go func() {
		je.wg.Wait()
		close(done)
	}()
	
	select {
	case <-done:
		log.Println("JourneyExecutor: All goroutines stopped cleanly")
	case <-time.After(30 * time.Second):
		log.Println("JourneyExecutor: Shutdown timeout - forcing stop")
	}
	
	je.deregisterWorker()
	
	log.Printf("JourneyExecutor: Stopped. Executed: %d, Errors: %d",
		atomic.LoadInt64(&je.totalExecuted), atomic.LoadInt64(&je.totalErrors))
}

// executionLoop is the main execution loop
func (je *JourneyExecutor) executionLoop() {
	defer je.wg.Done()
	
	ticker := time.NewTicker(je.pollInterval)
	defer ticker.Stop()
	
	for {
		select {
		case <-je.ctx.Done():
			return
		case <-ticker.C:
			je.processReadyEnrollments()
		}
	}
}

// processReadyEnrollments processes all enrollments ready for execution
func (je *JourneyExecutor) processReadyEnrollments() {
	ctx, cancel := context.WithTimeout(je.ctx, 30*time.Second)
	defer cancel()
	
	// Find enrollments ready to execute
	rows, err := je.db.QueryContext(ctx, `
		SELECT e.id, e.journey_id, e.subscriber_email, e.current_node_id, e.status, e.metadata
		FROM mailing_journey_enrollments e
		JOIN mailing_journeys j ON j.id = e.journey_id
		WHERE e.status = 'active'
		  AND j.status = 'active'
		  AND (e.next_execute_at IS NULL OR e.next_execute_at <= NOW())
		ORDER BY e.next_execute_at ASC NULLS FIRST
		LIMIT 100
		FOR UPDATE SKIP LOCKED
	`)
	if err != nil {
		if err != sql.ErrNoRows {
			log.Printf("JourneyExecutor: Error fetching enrollments: %v", err)
		}
		return
	}
	defer rows.Close()
	
	var enrollments []Enrollment
	for rows.Next() {
		var e Enrollment
		var metadataJSON sql.NullString
		err := rows.Scan(&e.ID, &e.JourneyID, &e.SubscriberEmail, &e.CurrentNodeID, &e.Status, &metadataJSON)
		if err != nil {
			continue
		}
		if metadataJSON.Valid {
			json.Unmarshal([]byte(metadataJSON.String), &e.Metadata)
		}
		if e.Metadata == nil {
			e.Metadata = make(map[string]interface{})
		}
		enrollments = append(enrollments, e)
	}
	
	// Process each enrollment
	for _, enrollment := range enrollments {
		if err := je.processEnrollment(ctx, enrollment); err != nil {
			atomic.AddInt64(&je.totalErrors, 1)
			log.Printf("JourneyExecutor: Error processing enrollment %s: %v", enrollment.ID, err)
		} else {
			atomic.AddInt64(&je.totalExecuted, 1)
		}
	}
}

// processEnrollment processes a single enrollment
func (je *JourneyExecutor) processEnrollment(ctx context.Context, enrollment Enrollment) error {
	// Get journey nodes
	var nodesJSON, connectionsJSON string
	err := je.db.QueryRowContext(ctx, `
		SELECT nodes, connections FROM mailing_journeys WHERE id = $1
	`, enrollment.JourneyID).Scan(&nodesJSON, &connectionsJSON)
	if err != nil {
		return fmt.Errorf("failed to get journey: %w", err)
	}
	
	var nodes []JourneyNodeExec
	var connections []JourneyConnectionExec
	json.Unmarshal([]byte(nodesJSON), &nodes)
	json.Unmarshal([]byte(connectionsJSON), &connections)
	
	// Find current node
	var currentNode *JourneyNodeExec
	for i := range nodes {
		if nodes[i].ID == enrollment.CurrentNodeID {
			currentNode = &nodes[i]
			break
		}
	}
	
	if currentNode == nil {
		// No current node, find first non-trigger node
		for i := range nodes {
			if nodes[i].Type != "trigger" {
				currentNode = &nodes[i]
				break
			}
		}
		if currentNode == nil {
			return je.completeEnrollment(ctx, enrollment.ID)
		}
	}
	
	// Execute the current node
	result, err := je.executeNode(ctx, enrollment, currentNode)
	if err != nil {
		je.logExecution(ctx, enrollment, currentNode, "error", err.Error())
		return err
	}
	
	je.logExecution(ctx, enrollment, currentNode, result.Action, "")
	
	// Handle result
	switch result.Action {
	case "wait":
		// Schedule next execution
		return je.scheduleNextExecution(ctx, enrollment.ID, result.WaitUntil)
	
	case "continue":
		// Move to next node
		nextNodeID := je.findNextNode(currentNode.ID, connections, result.Branch)
		if nextNodeID == "" {
			return je.completeEnrollment(ctx, enrollment.ID)
		}
		return je.moveToNode(ctx, enrollment.ID, nextNodeID)
	
	case "complete":
		return je.completeEnrollment(ctx, enrollment.ID)
	
	case "convert":
		return je.convertEnrollment(ctx, enrollment.ID)
	}
	
	return nil
}

// NodeExecutionResult contains the result of executing a node
type NodeExecutionResult struct {
	Action    string    // "wait", "continue", "complete", "convert"
	WaitUntil time.Time // For delay nodes
	Branch    string    // For condition/split nodes
}

// executeNode executes a journey node based on its type
func (je *JourneyExecutor) executeNode(ctx context.Context, enrollment Enrollment, node *JourneyNodeExec) (*NodeExecutionResult, error) {
	switch node.Type {
	case "trigger":
		// Triggers don't execute, just continue
		return &NodeExecutionResult{Action: "continue"}, nil
	
	case "email":
		return je.executeEmailNode(ctx, enrollment, node)
	
	case "delay":
		return je.executeDelayNode(ctx, enrollment, node)
	
	case "condition":
		return je.executeConditionNode(ctx, enrollment, node)
	
	case "split":
		return je.executeSplitNode(ctx, enrollment, node)
	
	case "goal":
		return &NodeExecutionResult{Action: "convert"}, nil
	
	default:
		return &NodeExecutionResult{Action: "continue"}, nil
	}
}

// executeEmailNode sends an email with personalization support
func (je *JourneyExecutor) executeEmailNode(ctx context.Context, enrollment Enrollment, node *JourneyNodeExec) (*NodeExecutionResult, error) {
	// Get email configuration from node (support both camelCase and snake_case)
	subject, _ := node.Config["subject"].(string)
	htmlContent, _ := node.Config["htmlContent"].(string)
	if htmlContent == "" {
		htmlContent, _ = node.Config["html_content"].(string) // snake_case fallback
	}
	fromName, _ := node.Config["fromName"].(string)
	if fromName == "" {
		fromName, _ = node.Config["from_name"].(string) // snake_case fallback
	}
	fromEmail, _ := node.Config["fromEmail"].(string)
	if fromEmail == "" {
		fromEmail, _ = node.Config["from_email"].(string) // snake_case fallback
	}
	templateID, _ := node.Config["templateId"].(string)
	if templateID == "" {
		templateID, _ = node.Config["template_id"].(string) // snake_case fallback
	}
	sendingProfileID, _ := node.Config["sending_profile_id"].(string)
	if sendingProfileID == "" {
		sendingProfileID, _ = node.Config["sendingProfileId"].(string) // camelCase fallback
	}
	
	// If sending profile ID provided, load profile settings
	if sendingProfileID != "" {
		var profileFromName, profileFromEmail sql.NullString
		err := je.db.QueryRowContext(ctx, `
			SELECT from_name, from_email 
			FROM mailing_sending_profiles WHERE id = $1
		`, sendingProfileID).Scan(&profileFromName, &profileFromEmail)
		if err == nil {
			if profileFromName.Valid && fromName == "" {
				fromName = profileFromName.String
			}
			if profileFromEmail.Valid && fromEmail == "" {
				fromEmail = profileFromEmail.String
			}
		}
	}
	
	// If template ID provided, load template
	if templateID != "" {
		err := je.db.QueryRowContext(ctx, `
			SELECT subject, html_content, from_name, from_email 
			FROM mailing_templates WHERE id = $1
		`, templateID).Scan(&subject, &htmlContent, &fromName, &fromEmail)
		if err != nil {
			return nil, fmt.Errorf("failed to load template: %w", err)
		}
	}
	
	// Defaults (use sending profile values or fallback)
	if fromName == "" {
		fromName = "IGNITE"
	}
	if fromEmail == "" {
		fromEmail = "noreply@ignite.media"
	}
	if subject == "" {
		subject = "Message from IGNITE"
	}
	if htmlContent == "" {
		htmlContent = "<p>Hello!</p>"
	}
	
	// ============================================
	// PERSONALIZATION ENGINE
	// ============================================
	templateSvc := mailing.NewTemplateService()
	contextBuilder := mailing.NewContextBuilder(je.db, "https://track.ignite.media", "signing-key-placeholder")
	
	// Load subscriber data for personalization
	sub := je.loadSubscriberByEmail(ctx, enrollment.SubscriberEmail)
	if sub != nil {
		// Build render context
		renderCtx, err := contextBuilder.BuildContext(ctx, sub, nil)
		if err != nil {
			log.Printf("JourneyExecutor: Failed to build context for %s: %v", enrollment.SubscriberEmail, err)
		} else {
			// Add journey-specific context
			renderCtx["journey"] = map[string]interface{}{
				"id":       enrollment.JourneyID,
				"node_id":  node.ID,
				"node_type": node.Type,
			}
			
			// Personalize content
			cacheKey := fmt.Sprintf("journey:%s:node:%s", enrollment.JourneyID, node.ID)
			
			personalizedSubject, _ := templateSvc.Render(cacheKey+":subject", subject, renderCtx)
			personalizedHTML, _ := templateSvc.Render(cacheKey+":html", htmlContent, renderCtx)
			
			subject = personalizedSubject
			htmlContent = personalizedHTML
		}
	}
	
	// Send the email
	if je.emailSender != nil {
		err := je.emailSender(ctx, enrollment.SubscriberEmail, subject, htmlContent, fromName, fromEmail)
		if err != nil {
			return nil, fmt.Errorf("failed to send email: %w", err)
		}
	} else {
		// Log for testing
		log.Printf("JourneyExecutor: Would send email to %s: subject=%s", enrollment.SubscriberEmail, subject)
	}
	
	return &NodeExecutionResult{Action: "continue"}, nil
}

// loadSubscriberByEmail loads subscriber data for personalization
func (je *JourneyExecutor) loadSubscriberByEmail(ctx context.Context, email string) *mailing.Subscriber {
	sub := &mailing.Subscriber{}
	
	err := je.db.QueryRowContext(ctx, `
		SELECT id, organization_id, list_id, email, email_hash, first_name, last_name,
			   status, source, ip_address, custom_fields, engagement_score,
			   total_emails_received, total_opens, total_clicks,
			   last_open_at, last_click_at, last_email_at,
			   optimal_send_hour_utc, timezone,
			   subscribed_at, unsubscribed_at, created_at, updated_at
		FROM mailing_subscribers
		WHERE LOWER(email) = LOWER($1)
		LIMIT 1
	`, email).Scan(
		&sub.ID, &sub.OrganizationID, &sub.ListID, &sub.Email, &sub.EmailHash,
		&sub.FirstName, &sub.LastName, &sub.Status, &sub.Source, &sub.IPAddress,
		&sub.CustomFields, &sub.EngagementScore,
		&sub.TotalEmailsReceived, &sub.TotalOpens, &sub.TotalClicks,
		&sub.LastOpenAt, &sub.LastClickAt, &sub.LastEmailAt,
		&sub.OptimalSendHourUTC, &sub.Timezone,
		&sub.SubscribedAt, &sub.UnsubscribedAt, &sub.CreatedAt, &sub.UpdatedAt,
	)
	
	if err != nil {
		if err != sql.ErrNoRows {
			log.Printf("JourneyExecutor: Error loading subscriber %s: %v", email, err)
		}
		return nil
	}
	
	return sub
}

// executeDelayNode handles delay nodes
func (je *JourneyExecutor) executeDelayNode(ctx context.Context, enrollment Enrollment, node *JourneyNodeExec) (*NodeExecutionResult, error) {
	// Check if we've already waited
	if _, waited := enrollment.Metadata["delay_started"]; waited {
		// Already waited, continue
		return &NodeExecutionResult{Action: "continue"}, nil
	}
	
	// Get delay configuration
	delayValue, _ := node.Config["delayValue"].(float64)
	delayUnit, _ := node.Config["delayUnit"].(string)
	
	if delayValue <= 0 {
		delayValue = 1
	}
	if delayUnit == "" {
		delayUnit = "hours"
	}
	
	// Calculate wait time
	var duration time.Duration
	switch delayUnit {
	case "minutes":
		duration = time.Duration(delayValue) * time.Minute
	case "hours":
		duration = time.Duration(delayValue) * time.Hour
	case "days":
		duration = time.Duration(delayValue) * 24 * time.Hour
	case "weeks":
		duration = time.Duration(delayValue) * 7 * 24 * time.Hour
	default:
		duration = time.Duration(delayValue) * time.Hour
	}
	
	waitUntil := time.Now().Add(duration)
	
	// Mark that we've started the delay
	if enrollment.Metadata == nil {
		enrollment.Metadata = make(map[string]interface{})
	}
	enrollment.Metadata["delay_started"] = true
	metadataJSON, _ := json.Marshal(enrollment.Metadata)
	je.db.ExecContext(ctx, `
		UPDATE mailing_journey_enrollments SET metadata = $2 WHERE id = $1
	`, enrollment.ID, string(metadataJSON))
	
	return &NodeExecutionResult{
		Action:    "wait",
		WaitUntil: waitUntil,
	}, nil
}

// executeConditionNode evaluates a condition
func (je *JourneyExecutor) executeConditionNode(ctx context.Context, enrollment Enrollment, node *JourneyNodeExec) (*NodeExecutionResult, error) {
	conditionType, _ := node.Config["conditionType"].(string)
	
	branch := "false" // Default to false branch
	
	switch conditionType {
	case "opened_email":
		// Check if subscriber opened any email in the journey
		var opened bool
		je.db.QueryRowContext(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM mailing_tracking_events 
				WHERE event_type = 'opened' 
				AND email = (SELECT email FROM mailing_subscribers WHERE email = $1 LIMIT 1)
			)
		`, enrollment.SubscriberEmail).Scan(&opened)
		if opened {
			branch = "true"
		}
	
	case "clicked_link":
		// Check if subscriber clicked any link
		var clicked bool
		je.db.QueryRowContext(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM mailing_tracking_events 
				WHERE event_type = 'clicked' 
				AND email = (SELECT email FROM mailing_subscribers WHERE email = $1 LIMIT 1)
			)
		`, enrollment.SubscriberEmail).Scan(&clicked)
		if clicked {
			branch = "true"
		}
	
	case "engagement_score":
		threshold, _ := node.Config["threshold"].(float64)
		var score float64
		je.db.QueryRowContext(ctx, `
			SELECT engagement_score FROM mailing_subscribers WHERE email = $1
		`, enrollment.SubscriberEmail).Scan(&score)
		if score >= threshold {
			branch = "true"
		}
	
	case "custom_field":
		fieldName, _ := node.Config["fieldName"].(string)
		expectedValue, _ := node.Config["fieldValue"].(string)
		
		var customFields map[string]interface{}
		var cfJSON string
		je.db.QueryRowContext(ctx, `
			SELECT custom_fields FROM mailing_subscribers WHERE email = $1
		`, enrollment.SubscriberEmail).Scan(&cfJSON)
		json.Unmarshal([]byte(cfJSON), &customFields)
		
		if actualValue, ok := customFields[fieldName]; ok {
			if fmt.Sprintf("%v", actualValue) == expectedValue {
				branch = "true"
			}
		}
	}
	
	return &NodeExecutionResult{
		Action: "continue",
		Branch: branch,
	}, nil
}

// executeSplitNode handles A/B split nodes
func (je *JourneyExecutor) executeSplitNode(ctx context.Context, enrollment Enrollment, node *JourneyNodeExec) (*NodeExecutionResult, error) {
	// Get split percentage (defaults to 50/50)
	splitPercentage, _ := node.Config["splitPercentage"].(float64)
	if splitPercentage <= 0 || splitPercentage >= 100 {
		splitPercentage = 50
	}
	
	// Use enrollment ID hash to determine branch (consistent assignment)
	hash := 0
	for _, c := range enrollment.ID {
		hash += int(c)
	}
	
	branch := "A"
	if hash%100 >= int(splitPercentage) {
		branch = "B"
	}
	
	return &NodeExecutionResult{
		Action: "continue",
		Branch: branch,
	}, nil
}

// findNextNode finds the next node based on connections
func (je *JourneyExecutor) findNextNode(currentNodeID string, connections []JourneyConnectionExec, branch string) string {
	for _, conn := range connections {
		if conn.From == currentNodeID {
			// If branch specified, match label
			if branch != "" && conn.Label != "" && conn.Label != branch {
				continue
			}
			return conn.To
		}
	}
	return ""
}

// scheduleNextExecution schedules the next execution time
func (je *JourneyExecutor) scheduleNextExecution(ctx context.Context, enrollmentID string, when time.Time) error {
	_, err := je.db.ExecContext(ctx, `
		UPDATE mailing_journey_enrollments 
		SET next_execute_at = $2, last_executed_at = NOW(), execution_count = execution_count + 1
		WHERE id = $1
	`, enrollmentID, when)
	return err
}

// moveToNode moves an enrollment to a new node
func (je *JourneyExecutor) moveToNode(ctx context.Context, enrollmentID, nodeID string) error {
	_, err := je.db.ExecContext(ctx, `
		UPDATE mailing_journey_enrollments 
		SET current_node_id = $2, next_execute_at = NOW(), last_executed_at = NOW(), 
		    execution_count = execution_count + 1, metadata = '{}'
		WHERE id = $1
	`, enrollmentID, nodeID)
	return err
}

// completeEnrollment marks an enrollment as completed
func (je *JourneyExecutor) completeEnrollment(ctx context.Context, enrollmentID string) error {
	_, err := je.db.ExecContext(ctx, `
		UPDATE mailing_journey_enrollments 
		SET status = 'completed', completed_at = NOW()
		WHERE id = $1
	`, enrollmentID)
	
	// Update journey stats
	je.db.ExecContext(ctx, `
		UPDATE mailing_journeys 
		SET total_completed = total_completed + 1 
		WHERE id = (SELECT journey_id FROM mailing_journey_enrollments WHERE id = $1)
	`, enrollmentID)
	
	return err
}

// convertEnrollment marks an enrollment as converted
func (je *JourneyExecutor) convertEnrollment(ctx context.Context, enrollmentID string) error {
	_, err := je.db.ExecContext(ctx, `
		UPDATE mailing_journey_enrollments 
		SET status = 'converted', completed_at = NOW()
		WHERE id = $1
	`, enrollmentID)
	
	// Update journey stats
	je.db.ExecContext(ctx, `
		UPDATE mailing_journeys 
		SET total_completed = total_completed + 1, total_converted = total_converted + 1
		WHERE id = (SELECT journey_id FROM mailing_journey_enrollments WHERE id = $1)
	`, enrollmentID)
	
	return err
}

// logExecution logs a node execution
func (je *JourneyExecutor) logExecution(ctx context.Context, enrollment Enrollment, node *JourneyNodeExec, action, errorMsg string) {
	result := "success"
	if errorMsg != "" {
		result = "error"
	}
	
	je.db.ExecContext(ctx, `
		INSERT INTO mailing_journey_execution_log 
		(id, enrollment_id, journey_id, node_id, node_type, action, result, error_message, executed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW())
	`, uuid.New(), enrollment.ID, enrollment.JourneyID, node.ID, node.Type, action, result, errorMsg)
}

// registerWorker registers this worker
func (je *JourneyExecutor) registerWorker() {
	je.db.Exec(`
		INSERT INTO mailing_workers (id, worker_type, hostname, status, started_at, last_heartbeat_at)
		VALUES ($1, 'journey_executor', $2, 'running', NOW(), NOW())
		ON CONFLICT (id) DO UPDATE SET
			status = 'running',
			started_at = NOW(),
			last_heartbeat_at = NOW()
	`, je.workerID, "ignite-worker")
}

// deregisterWorker removes this worker
func (je *JourneyExecutor) deregisterWorker() {
	je.db.Exec(`UPDATE mailing_workers SET status = 'stopped' WHERE id = $1`, je.workerID)
}

// heartbeatLoop sends periodic heartbeats
func (je *JourneyExecutor) heartbeatLoop() {
	defer je.wg.Done()
	
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	
	for {
		select {
		case <-je.ctx.Done():
			return
		case <-ticker.C:
			je.db.Exec(`
				UPDATE mailing_workers 
				SET last_heartbeat_at = NOW(), total_processed = $2, total_errors = $3
				WHERE id = $1
			`, je.workerID, atomic.LoadInt64(&je.totalExecuted), atomic.LoadInt64(&je.totalErrors))
		}
	}
}
