package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
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

	// Phase 3 (Welcome Series wave-native): when set, every email node
	// activation produces a tagged shadow campaign in mailing_campaigns
	// for observability and downstream advancer/watcher correlation.
	// Optional; nil keeps the legacy inline-send-only behavior so this
	// rollout is non-breaking.
	activator *JourneyEmailNodeActivator

	// Click-Drip (2026-06-01): when set, email nodes for click-postback
	// enrollments dispatch the reminder directly through PMTA on the
	// subscriber's original sending profile (tracking + message_log
	// included) instead of the generic emailSender callback. Optional;
	// nil falls back to emailSender so non-click-drip journeys are
	// unaffected.
	clickDripSender *JourneyClickDripSender
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
	ID              string
	JourneyID       string
	SubscriberEmail string
	CurrentNodeID   string
	Status          string
	NextExecuteAt   sql.NullTime
	Metadata        map[string]interface{}
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

// SetActivator wires the JourneyEmailNodeActivator. When configured,
// every email node activation also produces a tagged shadow campaign
// in mailing_campaigns. Wiring is opt-in so unit tests for executor
// branching unrelated to activation don't need to stand one up.
func (je *JourneyExecutor) SetActivator(a *JourneyEmailNodeActivator) {
	je.activator = a
}

// SetClickDripSender wires the dedicated PMTA reminder sender for
// click-postback enrollments. When set, email nodes whose enrollment
// originated from a click postback route through it (correct brand profile,
// tracking, message_log). Non-click-drip enrollments are unaffected.
func (je *JourneyExecutor) SetClickDripSender(s *JourneyClickDripSender) {
	je.clickDripSender = s
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

// claimLeaseSeconds is how far forward a claimed enrollment's next_execute_at
// is pushed while it is being processed. It must exceed the worst-case
// single-node processing time (a PMTA send round-trip) so a peer replica
// never re-claims an in-flight enrollment, yet be short enough that a crashed
// executor's enrollment retries promptly. The terminal node outcome
// (wait/continue/complete/error) overwrites this lease before it elapses in
// the happy path; on an errored send the lease becomes the retry backoff.
const claimLeaseSeconds = 120

// perEnrollmentTimeout is the processing budget for a SINGLE enrollment
// (claim → node execution → send). It is deliberately separate from the
// candidate-listing budget: the previous shared 30s batch context was
// consumed by the upstream query chain (subscriber load + context build +
// lookups) under DB load — especially during segment-rebuild storms that
// contend for the app's connection pool — leaving ~0ms for the actual PMTA
// send, which then failed with "context deadline exceeded" at the first send
// query (ensureShadowCampaign). A per-enrollment budget guarantees each send
// a full window regardless of batch size or upstream slowness. It MUST stay
// below claimLeaseSeconds so a peer replica never re-claims an in-flight row.
const perEnrollmentTimeout = 90 * time.Second

// processReadyEnrollments processes all enrollments ready for execution.
//
// Concurrency model (fixed 2026-06-01): the executor runs in BOTH server
// replicas. The previous implementation selected due rows with
// `FOR UPDATE SKIP LOCKED` but did NOT hold a surrounding transaction, so the
// row lock was released the instant the rows were scanned. Two replicas could
// therefore process the SAME enrollment in the same millisecond — and because
// delay nodes use a `delay_started` metadata flag set on the first pass and
// honored on the second, the double-pass made the second processor skip the
// delay entirely (reminders fired immediately instead of +1h/+6h/+24h/+72h).
//
// Fix: claim each due enrollment with a single atomic UPDATE that leases
// next_execute_at forward. Only one replica's UPDATE can match the
// `next_execute_at <= NOW()` predicate, guaranteeing exactly-once processing
// per due-cycle without threading a *sql.Tx through every node helper.
func (je *JourneyExecutor) processReadyEnrollments() {
	// 1. Find candidate ids (cheap, no lock) under a short listing budget.
	//    The authoritative due-check and claim happen atomically in step 2.
	listCtx, listCancel := context.WithTimeout(je.ctx, 30*time.Second)
	rows, err := je.db.QueryContext(listCtx, `
		SELECT e.id
		FROM mailing_journey_enrollments e
		JOIN mailing_journeys j ON j.id = e.journey_id
		WHERE e.status = 'active'
		  AND j.status = 'active'
		  AND (e.next_execute_at IS NULL OR e.next_execute_at <= NOW())
		ORDER BY e.next_execute_at ASC NULLS FIRST
		LIMIT 100
	`)
	if err != nil {
		listCancel()
		if err != sql.ErrNoRows {
			log.Printf("JourneyExecutor: Error fetching enrollments: %v", err)
		}
		return
	}
	var candidateIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			continue
		}
		candidateIDs = append(candidateIDs, id)
	}
	rows.Close()
	listCancel()

	// 2. Atomically claim + process each candidate under its OWN budget so a
	//    slow/contended enrollment can't starve the rest (and no enrollment
	//    inherits a near-expired shared batch context). A claim that returns
	//    no row means a peer replica grabbed it first — skip silently.
	for _, id := range candidateIDs {
		procCtx, procCancel := context.WithTimeout(je.ctx, perEnrollmentTimeout)
		enrollment, claimed := je.claimEnrollment(procCtx, id)
		if !claimed {
			procCancel()
			continue
		}
		if err := je.processEnrollment(procCtx, enrollment); err != nil {
			atomic.AddInt64(&je.totalErrors, 1)
			log.Printf("JourneyExecutor: Error processing enrollment %s: %v", enrollment.ID, err)
		} else {
			atomic.AddInt64(&je.totalExecuted, 1)
		}
		procCancel()
	}
}

// claimEnrollment atomically leases a due enrollment so exactly one executor
// (across replicas) processes it this cycle. Returns claimed=false when the
// row was already claimed/advanced by a peer or is no longer due.
func (je *JourneyExecutor) claimEnrollment(ctx context.Context, id string) (Enrollment, bool) {
	var e Enrollment
	var metadataJSON sql.NullString
	err := je.db.QueryRowContext(ctx, `
		UPDATE mailing_journey_enrollments
		SET next_execute_at = NOW() + make_interval(secs => $2)
		WHERE id = $1
		  AND status = 'active'
		  AND (next_execute_at IS NULL OR next_execute_at <= NOW())
		RETURNING id, journey_id, subscriber_email, current_node_id, status, metadata
	`, id, claimLeaseSeconds).Scan(
		&e.ID, &e.JourneyID, &e.SubscriberEmail, &e.CurrentNodeID, &e.Status, &metadataJSON,
	)
	if err != nil {
		if err != sql.ErrNoRows {
			log.Printf("JourneyExecutor: claim enrollment %s: %v", id, err)
		}
		return Enrollment{}, false
	}
	if metadataJSON.Valid {
		json.Unmarshal([]byte(metadataJSON.String), &e.Metadata)
	}
	if e.Metadata == nil {
		e.Metadata = make(map[string]interface{})
	}
	return e, true
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

		// RETRY POLICY (2026-08-25). Before this, an error returned here and the
		// claim lease alone decided when to try again — a ~2-minute hot loop
		// that ran for 13 days on three mailboxes and wrote 26,908 log rows.
		// Now every failure is classified, backed off, and eventually ejected
		// with a reason code. See journey_retry.go for the measured incident.
		d := recordJourneySendFailure(ctx, je.db, enrollment.ID, currentNode.ID, err)
		if d.Terminal {
			ejectJourneyEnrollment(ctx, je.db, enrollment.ID, currentNode.ID, d.Reason, err)
			je.logExecution(ctx, enrollment, currentNode, "terminal_failure", d.Reason+": "+err.Error())
		} else {
			deferJourneyEnrollment(ctx, je.db, enrollment.ID, d.NextDelay)
		}
		return err
	}

	// A node that executed clears its retry history — a lane that failed twice
	// at touch 2 must not carry that count into touch 3.
	clearJourneyRetry(ctx, je.db, enrollment.ID)

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

	// ────────────────────────────────────────────────────────────────────
	// Click-Drip Journey override (Phase 3, 2026-06-01).
	//
	// When the enrollment was created by the click-postback pipeline
	// (JourneyEventEnroller), enrollment.Metadata carries everything we
	// need to make this reminder feel like a continuation of the
	// subscriber's original click:
	//   - sending_profile_id: brand domain they clicked from
	//   - body_html:          the exact creative they clicked
	//   - original_from_name: friendly-from on the original send
	//   - original_from_email: from-email on the original send
	//
	// Body HTML reuse is operator policy: the reminder is a "did you
	// forget?" continuation of the same creative, not a re-pitch. Subject
	// + preheader come from mailing_offer_reminder_subjects below.
	//
	// Existing (non-click-drip) journeys have Metadata without these
	// keys → all overrides are no-ops → behaviour is preserved.
	// ────────────────────────────────────────────────────────────────────
	if mdProfile, ok := enrollment.Metadata["sending_profile_id"].(string); ok && mdProfile != "" {
		sendingProfileID = mdProfile
	}
	if mdBody, ok := enrollment.Metadata["body_html"].(string); ok && mdBody != "" {
		htmlContent = mdBody
	}
	if mdFromName, ok := enrollment.Metadata["original_from_name"].(string); ok && mdFromName != "" && fromName == "" {
		fromName = mdFromName
	}
	if mdFromEmail, ok := enrollment.Metadata["original_from_email"].(string); ok && mdFromEmail != "" && fromEmail == "" {
		fromEmail = mdFromEmail
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

	// ────────────────────────────────────────────────────────────────────
	// Click-Drip reminder subject override (Phase 3, 2026-06-01).
	//
	// For click-drip journeys we want a different subject + preheader
	// per reminder step (e.g. step 0 = "+1h: Did you forget?", step 3 =
	// "+72h: Last chance"). The journey's email node carries
	// reminder_sequence_index (0..N-1) and the click-postback
	// enrollment carries everflow_offer_id in metadata. Together they
	// key into mailing_offer_reminder_subjects.
	//
	// Body HTML is intentionally NOT overridden — operator policy
	// 2026-06-01 is to reuse the *same creative* the subscriber
	// originally clicked, so the reminder feels like a continuation,
	// not a brand-new offer pitch.
	// ────────────────────────────────────────────────────────────────────
	// touchPreheader / touchContentVersion carry this touch's creative identity
	// down to the sender: the hash keys the shadow campaign, which is what makes
	// each creative version's metrics separate (and freezes the previous
	// version's as a historical aggregate).
	touchPreheader := ""
	touchContentVersion := ""
	if reminderSeq, ok := readReminderSeqIndex(node.Config); ok {
		if everflowOfferID, ok := enrollment.Metadata["everflow_offer_id"].(string); ok && everflowOfferID != "" {
			var rSubject, rPreheader, rFromOverride, rBody, rProofID sql.NullString
			var rEnabled sql.NullBool
			// body_html ships with a migration that can lag this binary. If the
			// column is absent the 5-column read errors, and the `err == nil`
			// guard below would silently DROP the subject/preheader/from
			// overrides too — every touch would quietly revert to generic copy.
			// Fall back to the pre-migration shape instead.
			err := je.db.QueryRowContext(ctx, `
				SELECT subject, preheader, from_name_override, enabled, body_html, proof_id::text
				FROM mailing_offer_reminder_subjects
				WHERE everflow_offer_id=$1 AND sequence_index=$2
			`, everflowOfferID, reminderSeq).Scan(&rSubject, &rPreheader, &rFromOverride, &rEnabled, &rBody, &rProofID)
			if err != nil && strings.Contains(err.Error(), "does not exist") {
				err = je.db.QueryRowContext(ctx, `
					SELECT subject, preheader, from_name_override, enabled
					FROM mailing_offer_reminder_subjects
					WHERE everflow_offer_id=$1 AND sequence_index=$2
				`, everflowOfferID, reminderSeq).Scan(&rSubject, &rPreheader, &rFromOverride, &rEnabled)
				rBody, rProofID = sql.NullString{}, sql.NullString{}
			}
			if err == nil && rEnabled.Valid && rEnabled.Bool {
				if rSubject.Valid && rSubject.String != "" {
					subject = rSubject.String
				}
				if rPreheader.Valid && rPreheader.String != "" {
					touchPreheader = rPreheader.String
					if node.Config == nil {
						node.Config = map[string]interface{}{}
					}
					node.Config["preheader"] = rPreheader.String
				}
				if rFromOverride.Valid && rFromOverride.String != "" {
					fromName = rFromOverride.String
				}
				// Per-touch BODY override (2026-08-02). The original policy was
				// to reuse whatever creative the subscriber clicked so the
				// reminder felt like a continuation — but that let an August
				// body ship under a June subject (offer 420 paired "Ends 7/5"
				// with Aug-1 partner-drip creatives). When an operator sets a
				// body for the touch it wins, making the touch a coherent unit.
				// Creative Studio → OFFERS (mailing_offer_proofs) is the source
				// of truth for an offer lane's creative. Only an APPROVED and
				// ACTIVE proof may send: the platform's approved-copy rule is a
				// hard gate, so a proof that was withdrawn or un-approved after
				// selection must NOT keep mailing — it falls through to the
				// snapshot instead of shipping unapproved advertiser copy.
				//
				// Resolving live (rather than snapshotting at selection) means a
				// proof edit reaches the drip, and because the version hash below
				// covers the resolved HTML it also mints a new creative version
				// and sunsets the previous one's metrics.
				if rProofID.Valid && rProofID.String != "" {
					var pHTML sql.NullString
					var pApproved sql.NullString
					var pActive sql.NullBool
					perr := je.db.QueryRowContext(ctx, `
						SELECT html_content, approval_status, is_active
						FROM mailing_offer_proofs WHERE id = $1::uuid
					`, rProofID.String).Scan(&pHTML, &pApproved, &pActive)
					switch {
					case perr != nil:
						log.Printf("JourneyExecutor: offer proof %s unresolved (offer %s seq %d): %v",
							rProofID.String, everflowOfferID, reminderSeq, perr)
					case !pActive.Valid || !pActive.Bool || !strings.EqualFold(pApproved.String, "approved"):
						log.Printf("JourneyExecutor: offer proof %s is not approved+active (status=%q active=%v) — NOT sending it (offer %s seq %d)",
							rProofID.String, pApproved.String, pActive.Bool, everflowOfferID, reminderSeq)
					case pHTML.Valid && strings.TrimSpace(pHTML.String) != "":
						htmlContent = pHTML.String
					}
				}
				if htmlContent == "" && rBody.Valid && strings.TrimSpace(rBody.String) != "" {
					htmlContent = rBody.String
				}
				touchContentVersion = touchContentHash(subject, touchPreheader, fromName, htmlContent)
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
		fromName = "Notifications"
	}
	if fromEmail == "" {
		fromEmail = "noreply@ignite.media"
	}
	if subject == "" {
		subject = "You have a new message"
	}
	if htmlContent == "" {
		htmlContent = "<p>Hello!</p>"
	}

	// ============================================
	// PERSONALIZATION ENGINE
	// ============================================
	templateSvc := mailing.NewTemplateService()
	trackURL := os.Getenv("TRACKING_URL")
	if trackURL == "" {
		trackURL = "https://track.ignite.media"
	}
	sigKey := os.Getenv("TRACKING_SIGNING_KEY")
	if sigKey == "" {
		sigKey = "jarvis-default-signing-key-change-in-production"
	}
	contextBuilder := mailing.NewContextBuilder(je.db, trackURL, sigKey)

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
				"id":        enrollment.JourneyID,
				"node_id":   node.ID,
				"node_type": node.Type,
			}

			// Click-drip: BuildContext above ran with campaign=nil, so the
			// system.* URLs (unsubscribe/preferences/…) were never populated
			// and the lax Liquid render below would silently strip
			// {{ system.unsubscribe_url }} et al. from the creative footer —
			// shipping a reminder with no working unsubscribe link. Populate
			// them broadcast-style before rendering (parity with
			// SendWorkerPool.buildRenderContext).
			je.mergeClickDripSystemURLs(ctx, renderCtx, enrollment, node.ID, touchContentVersion, sub.ID.String(), sendingProfileID, fromEmail)

			// Personalize content
			cacheKey := fmt.Sprintf("journey:%s:node:%s", enrollment.JourneyID, node.ID)

			personalizedSubject, _ := templateSvc.Render(cacheKey+":subject", subject, renderCtx)
			personalizedHTML, _ := templateSvc.Render(cacheKey+":html", htmlContent, renderCtx)

			subject = personalizedSubject
			htmlContent = personalizedHTML
		}
	}

	// Phase 3 (Welcome Series wave-native): buffer this activation into
	// a shadow campaign for /node-stats observability + Phase 4
	// engagement-watcher correlation. Failures here are non-fatal: we
	// log and fall back to the legacy inline send so journey emails do
	// not stall on activator infrastructure issues.
	if je.activator != nil {
		if err := je.activator.ActivateNode(
			enrollment.ID, enrollment.JourneyID, node.ID, enrollment.SubscriberEmail,
			subject, htmlContent, fromName, fromEmail,
		); err != nil {
			log.Printf("JourneyExecutor: activator.ActivateNode failed (journey=%s node=%s): %v",
				enrollment.JourneyID, node.ID, err)
		}
	}

	// ────────────────────────────────────────────────────────────────────
	// Click-Drip dispatch (2026-06-01): when this enrollment originated
	// from a click postback AND a clickDripSender is wired, send the
	// reminder directly through PMTA on the subscriber's original sending
	// profile. This path does tracking rewrite + writes mailing_message_log
	// and is the production send path for click-drip reminders.
	// ────────────────────────────────────────────────────────────────────
	if je.clickDripSender != nil && isClickDripEnrollment(enrollment) {
		everflowOfferID, _ := enrollment.Metadata["everflow_offer_id"].(string)
		reminderSeq, _ := readReminderSeqIndex(node.Config)

		subscriberID := ""
		if sub != nil && sub.ID != uuid.Nil {
			subscriberID = sub.ID.String()
		} else if orig, ok := enrollment.Metadata["original_subscriber"].(string); ok {
			subscriberID = orig
		}

		err := je.clickDripSender.Send(ctx, ClickDripSendParams{
			JourneyID:       enrollment.JourneyID,
			NodeID:          node.ID,
			EverflowOfferID: everflowOfferID,
			ReminderSeq:     reminderSeq,
			SubscriberID:    subscriberID,
			SubscriberEmail: enrollment.SubscriberEmail,
			Subject:         subject,
			HTMLContent:     htmlContent,
			FromName:        fromName,
			FromEmail:       fromEmail,
			ProfileID:       sendingProfileID,
			// ContentHash was computed but NEVER passed (verified 2026-08-25:
			// mailing_clickdrip_touch_versions was empty estate-wide and every
			// production shadow campaign carried the hashless id). Passing it
			// makes creative versioning real. shadowCampaignID keeps the
			// hashless id as version 0 so pre-cutover metrics stay attached —
			// see ensureShadowCampaign.
			ContentHash: touchContentVersion,
		})
		if err != nil {
			return nil, fmt.Errorf("click-drip send failed: %w", err)
		}
		return &NodeExecutionResult{Action: "continue"}, nil
	}

	// Send the email (legacy inline path; will be replaced by the
	// shadow-campaign-driven PMTA wave pipeline in Phase 3.5).
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

// isClickDripEnrollment reports whether an enrollment came from the
// click-postback pipeline. We key off enrolled_via (set by the enroller) and
// fall back to the presence of a sending_profile_id in metadata.
func isClickDripEnrollment(e Enrollment) bool {
	// Prefix match covers "click_postback" AND "click_postback_rearm" (the
	// re-enrollment path, 2026-07-10) — a re-armed row must not depend
	// solely on the metadata["source"] fallback to reach the sender.
	if v, ok := e.Metadata["enrolled_via"].(string); ok && strings.HasPrefix(v, "click_postback") {
		return true
	}
	if v, ok := e.Metadata["source"].(string); ok && v == "click_postback" {
		return true
	}
	if v, ok := e.Metadata["sending_profile_id"].(string); ok && v != "" {
		return true
	}
	return false
}

// mergeClickDripSystemURLs merges the broadcast-parity {{ system.* }} URL
// values into a click-drip enrollment's render context so the Liquid render
// resolves the creative's unsubscribe/preferences footer links to REAL signed
// URLs (the same generators the campaign send worker uses) instead of
// stripping them to empty strings. No-op for non-click-drip enrollments and
// when no clickDripSender is wired, so legacy journeys are untouched.
func (je *JourneyExecutor) mergeClickDripSystemURLs(ctx context.Context, renderCtx mailing.RenderContext, enrollment Enrollment, nodeID, contentHash, subscriberID, profileID, fromEmail string) {
	if je.clickDripSender == nil || !isClickDripEnrollment(enrollment) {
		return
	}
	sysCtx, ok := renderCtx["system"].(map[string]interface{})
	if !ok {
		return
	}
	everflowOfferID, _ := enrollment.Metadata["everflow_offer_id"].(string)
	for k, v := range je.clickDripSender.SystemURLs(ctx, everflowOfferID, nodeID, contentHash, subscriberID, profileID, fromEmail) {
		sysCtx[k] = v
	}
}

// loadSubscriberByEmail loads subscriber data for personalization
func (je *JourneyExecutor) loadSubscriberByEmail(ctx context.Context, email string) *mailing.Subscriber {
	sub := &mailing.Subscriber{}

	// NULL-safe scan: mailing.Subscriber's FirstName/LastName/Status/Source/
	// IPAddress/Timezone are plain (non-pointer) strings, but every one of
	// those columns is nullable in mailing_subscribers — a NULL first_name
	// crashed every click-drip personalization load with `sql: Scan error on
	// column index 5 ... converting NULL to string is unsupported` (prod
	// 2026-07). COALESCE exactly those columns to ''; pointer / sql-Scanner
	// fields (last_open_at, custom_fields, …) already handle NULL themselves.
	err := je.db.QueryRowContext(ctx, `
		SELECT id, organization_id, list_id, email, email_hash,
			   COALESCE(first_name, ''), COALESCE(last_name, ''),
			   COALESCE(status, ''), COALESCE(source, ''), COALESCE(ip_address::text, ''),
			   custom_fields, engagement_score,
			   total_emails_received, total_opens, total_clicks,
			   last_open_at, last_click_at, last_email_at,
			   optimal_send_hour_utc, COALESCE(timezone, ''),
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

// executeDelayNode handles delay nodes.
//
// Phase 2 (Welcome Series) extends the original "fixed duration" delay
// with a "until specific time of day" mode that respects a timezone
// chosen in the builder UI. The supported delayType values are:
//
//   - ""  / "fixed"     (default): wait delayValue * delayUnit
//   - "until_time":     wait until the next HH:MM in untilTimezone
//   - "until_day":      reserved for a future iteration; today behaves
//     like "fixed days" with delayValue=1 to preserve
//     the previous semantics.
//
// untilTime is "HH:MM" 24h. untilTimezone is a Go time.LoadLocation name
// (e.g. America/Denver). Invalid timezone falls back to UTC and is logged
// so we never block the executor.
func (je *JourneyExecutor) executeDelayNode(ctx context.Context, enrollment Enrollment, node *JourneyNodeExec) (*NodeExecutionResult, error) {
	if _, waited := enrollment.Metadata["delay_started"]; waited {
		return &NodeExecutionResult{Action: "continue"}, nil
	}

	delayType, _ := node.Config["delayType"].(string)
	waitUntil := computeDelayWaitUntil(node.Config, delayType, time.Now())

	if enrollment.Metadata == nil {
		enrollment.Metadata = make(map[string]interface{})
	}
	enrollment.Metadata["delay_started"] = true
	enrollment.Metadata["delay_until"] = waitUntil.UTC().Format(time.RFC3339)
	metadataJSON, _ := json.Marshal(enrollment.Metadata)
	je.db.ExecContext(ctx, `
		UPDATE mailing_journey_enrollments SET metadata = $2 WHERE id = $1
	`, enrollment.ID, string(metadataJSON))

	return &NodeExecutionResult{
		Action:    "wait",
		WaitUntil: waitUntil,
	}, nil
}

// computeDelayWaitUntil is a pure function so it can be unit tested
// without standing up the executor + DB. It mirrors every supported
// delayType branch above.
// readReminderSeqIndex extracts the reminder sequence index (0..N) from
// a journey email node's config. Used by the click-drip pipeline to key
// into mailing_offer_reminder_subjects per step. Accepts both
// reminder_sequence_index (snake_case) and reminderSequenceIndex
// (camelCase). Numeric (json.Number / float64) and string forms are
// both honored. Returns ok=false when no usable value is present.
func readReminderSeqIndex(config map[string]interface{}) (int, bool) {
	if config == nil {
		return 0, false
	}
	tryKeys := []string{"reminder_sequence_index", "reminderSequenceIndex"}
	for _, k := range tryKeys {
		raw, present := config[k]
		if !present {
			continue
		}
		switch v := raw.(type) {
		case float64:
			return int(v), true
		case int:
			return v, true
		case int64:
			return int(v), true
		case json.Number:
			if i, err := v.Int64(); err == nil {
				return int(i), true
			}
		case string:
			if v == "" {
				continue
			}
			if i, err := strconv.Atoi(v); err == nil {
				return i, true
			}
		}
	}
	return 0, false
}

func computeDelayWaitUntil(config map[string]interface{}, delayType string, now time.Time) time.Time {
	switch delayType {
	case "until_time":
		return computeUntilTime(config, now)
	default:
		// "fixed" or unknown: legacy behavior.
		delayValue, _ := config["delayValue"].(float64)
		delayUnit, _ := config["delayUnit"].(string)
		if delayValue <= 0 {
			delayValue = 1
		}
		if delayUnit == "" {
			delayUnit = "hours"
		}
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
		return now.Add(scaleJourneyDelay(duration))
	}
}

// scaleJourneyDelay applies the optional JOURNEY_DELAY_SCALE test knob.
//
// JOURNEY_DELAY_SCALE is a float multiplier applied to every FIXED journey
// delay. Unset, empty, non-numeric, or <= 0 → no scaling (production
// default, exactly the legacy behavior). Example: "0.001" turns a 1h delay
// into 3.6s so an end-to-end click-drip run can be observed in minutes
// rather than 72h. There is also a floor of 5s so accelerated delays never
// collapse to zero and starve the executor's wait scheduling.
func scaleJourneyDelay(d time.Duration) time.Duration {
	raw := os.Getenv("JOURNEY_DELAY_SCALE")
	if raw == "" {
		return d
	}
	scale, err := strconv.ParseFloat(raw, 64)
	if err != nil || scale <= 0 {
		return d
	}
	scaled := time.Duration(float64(d) * scale)
	if scaled < 5*time.Second {
		scaled = 5 * time.Second
	}
	return scaled
}

// computeUntilTime resolves the next HH:MM in the configured timezone.
// If now is already past today's HH:MM, we roll to tomorrow. The returned
// time is the wall-clock instant in UTC so the executor's
// scheduleNextExecution can compare against time.Now() directly.
func computeUntilTime(config map[string]interface{}, now time.Time) time.Time {
	hhmm, _ := config["untilTime"].(string)
	tzName, _ := config["untilTimezone"].(string)

	if hhmm == "" {
		hhmm = "09:00"
	}
	if tzName == "" {
		tzName = "America/Denver"
	}

	loc, err := time.LoadLocation(tzName)
	if err != nil {
		// Fall back to UTC so we never block. Caller-side logging is fine.
		loc = time.UTC
	}

	parsed, err := time.Parse("15:04", hhmm)
	if err != nil {
		// Default to 9am if the operator typed something invalid.
		parsed, _ = time.Parse("15:04", "09:00")
	}

	nowInTZ := now.In(loc)
	target := time.Date(
		nowInTZ.Year(), nowInTZ.Month(), nowInTZ.Day(),
		parsed.Hour(), parsed.Minute(), 0, 0, loc,
	)
	if !target.After(nowInTZ) {
		target = target.Add(24 * time.Hour)
	}
	return target.UTC()
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

// moveToNode moves an enrollment to a new node.
//
// Metadata handling (revised 2026-06-01 for the Click-Drip Journey):
//
// Originally this function wiped metadata to '{}' on every transition.
// That worked for the Welcome Series because the only metadata anyone
// wrote was the delay-tracking scratch (`delay_started`, `delay_until`)
// which MUST clear between delay nodes. But the click-drip pipeline
// stamps stable per-enrollment context at enrollment time
// (sending_profile_id, body_html, original_from_*, everflow_offer_id)
// that the email-node executor reads on EVERY downstream node — so a
// blanket wipe would lose it.
//
// New behavior: remove only the per-node scratch keys. Postgres'
// `jsonb - text` operator strips a single key cleanly, and chaining
// covers both. Anything else stamped at enrollment time (or by the
// activator's later jsonb_set on shadow_campaign_id) survives the
// transition unchanged. delay_started/delay_until still clear between
// delay nodes as before.
func (je *JourneyExecutor) moveToNode(ctx context.Context, enrollmentID, nodeID string) error {
	_, err := je.db.ExecContext(ctx, `
		UPDATE mailing_journey_enrollments
		SET current_node_id = $2,
		    next_execute_at = NOW(),
		    last_executed_at = NOW(),
		    execution_count = execution_count + 1,
		    metadata = COALESCE(metadata, '{}'::jsonb) - 'delay_started' - 'delay_until'
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
