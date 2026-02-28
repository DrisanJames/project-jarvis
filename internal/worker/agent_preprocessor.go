package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// =============================================================================
// AGENT PREPROCESSOR — "Traffic Cop" Service
// =============================================================================
// Runs before the send worker to pre-process a campaign's audience through
// ISP agents. Each agent evaluates its recipients and writes per-recipient
// decisions into the database and Redis for the send worker to consume.

// AgentPreprocessor orchestrates ISP agent evaluation of campaign audiences.
type AgentPreprocessor struct {
	db    *sql.DB
	redis *redis.Client
}

// NewAgentPreprocessor creates a new preprocessor instance.
func NewAgentPreprocessor(db *sql.DB, redis *redis.Client) *AgentPreprocessor {
	return &AgentPreprocessor{db: db, redis: redis}
}

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

// AgentDecision holds the per-recipient send decision produced by an agent.
type AgentDecision struct {
	EmailHash       string                 `json:"email_hash"`
	Classification  string                 `json:"classification"`   // send_now, send_later, defer, suppress
	ContentStrategy string                 `json:"content_strategy"` // text_personalized, text_generic, image_personalized, image_generic
	OptimalSendHour int                    `json:"optimal_send_hour"`
	Priority        int                    `json:"priority"` // 0-100
	Reasoning       map[string]interface{} `json:"reasoning,omitempty"`
}

// ispAgent mirrors a row from mailing_isp_agents.
type ispAgent struct {
	ID        uuid.UUID
	ISP       string
	Domain    string
	Status    string
	Config    json.RawMessage
	Knowledge json.RawMessage
}

// agentCampaign mirrors a row from mailing_agent_campaigns.
type agentCampaign struct {
	ID             uuid.UUID
	AgentID        uuid.UUID
	CampaignID     uuid.UUID
	RecipientCount int
	Status         string
	SendWindow     json.RawMessage
}

// inboxProfile mirrors the columns we read from mailing_inbox_profiles.
type inboxProfile struct {
	EmailHash            string
	EngagementScore      float64
	EngagementTrend      sql.NullString
	TotalSends           int
	TotalOpens           int
	TotalClicks          int
	TotalBounces         int
	TotalComplaints      int
	OptimalSendHour      int
	PrefTextScore        float64
	PrefImageScore       float64
	PrefPersonalizedScore float64
	InboxHealth          sql.NullString
	SendSuspendedUntil   sql.NullTime
	MonthlyEngagement    json.RawMessage
	RevenueTotal         float64
	ConsecutiveBounces   int
	LastOpenAt           sql.NullTime
	LastClickAt          sql.NullTime
}

// decisionRedisPayload is the slim JSON stored per-recipient in Redis.
type decisionRedisPayload struct {
	Classification  string `json:"classification"`
	ContentStrategy string `json:"content_strategy"`
	Priority        int    `json:"priority"`
	OptimalSendHour int    `json:"optimal_send_hour"`
}

// campaignSummary counts decisions by classification.
type campaignSummary struct {
	SendNow   int `json:"send_now"`
	SendLater int `json:"send_later"`
	Defer     int `json:"defer"`
	Suppress  int `json:"suppress"`
	Total     int `json:"total"`
}

// ---------------------------------------------------------------------------
// 1. PreprocessCampaign — main entry point
// ---------------------------------------------------------------------------

// PreprocessCampaign evaluates every recipient assigned to ISP agents for the
// given campaign and writes per-recipient decisions to the DB and Redis.
// Returns nil immediately when no agents are assigned (normal send path).
func (ap *AgentPreprocessor) PreprocessCampaign(ctx context.Context, campaignID uuid.UUID) error {
	// 1. Load agent-campaign assignments.
	assignments, err := ap.loadAgentCampaigns(ctx, campaignID)
	if err != nil {
		return fmt.Errorf("agent preprocessor: load assignments: %w", err)
	}
	if len(assignments) == 0 {
		return nil // no agents — campaign sends normally
	}

	log.Printf("[AgentPreprocessor] Campaign %s: found %d agent assignment(s)", campaignID, len(assignments))

	totalDecisions := 0

	for i := range assignments {
		ac := &assignments[i]

		// Load the ISP agent row.
		agent, err := ap.loadAgent(ctx, ac.AgentID)
		if err != nil {
			return fmt.Errorf("agent preprocessor: load agent %s: %w", ac.AgentID, err)
		}

		count, err := ap.processAgentAudience(ctx, agent, ac)
		if err != nil {
			return fmt.Errorf("agent preprocessor: process agent %s (%s): %w", agent.ISP, agent.Domain, err)
		}

		// Mark the assignment as active.
		_, err = ap.db.ExecContext(ctx,
			`UPDATE mailing_agent_campaigns SET status = 'active', started_at = NOW() WHERE id = $1`,
			ac.ID,
		)
		if err != nil {
			return fmt.Errorf("agent preprocessor: activate assignment %s: %w", ac.ID, err)
		}

		totalDecisions += count
	}

	log.Printf("[AgentPreprocessor] Campaign %s: %d total decisions written", campaignID, totalDecisions)
	return nil
}

// ---------------------------------------------------------------------------
// 2. processAgentAudience
// ---------------------------------------------------------------------------

// processAgentAudience loads inbox profiles for the agent's domain, classifies
// each recipient, batch-inserts decisions into the DB, and writes them to Redis.
func (ap *AgentPreprocessor) processAgentAudience(ctx context.Context, agent *ispAgent, ac *agentCampaign) (int, error) {
	profiles, err := ap.loadInboxProfiles(ctx, agent.Domain)
	if err != nil {
		return 0, fmt.Errorf("load profiles for %s: %w", agent.Domain, err)
	}

	if len(profiles) == 0 {
		log.Printf("[AgentPreprocessor] Agent %s (%s): no inbox profiles found", agent.ISP, agent.Domain)
		return 0, nil
	}

	// Classify every recipient.
	decisions := make([]AgentDecision, 0, len(profiles))
	counts := campaignSummary{}

	for i := range profiles {
		d := classifyRecipient(&profiles[i], agent.Knowledge)
		d.EmailHash = profiles[i].EmailHash
		decisions = append(decisions, d)

		switch d.Classification {
		case "send_now":
			counts.SendNow++
		case "send_later":
			counts.SendLater++
		case "defer":
			counts.Defer++
		case "suppress":
			counts.Suppress++
		}
		counts.Total++
	}

	log.Printf("[AgentPreprocessor] Agent %s (%s) pre-processing: %d profiles loaded, %d send_now, %d send_later, %d defer, %d suppress",
		agent.ISP, agent.Domain, len(profiles), counts.SendNow, counts.SendLater, counts.Defer, counts.Suppress)

	// Batch insert decisions into the database.
	if err := ap.batchInsertDecisions(ctx, agent.ID, ac.CampaignID, decisions); err != nil {
		return 0, fmt.Errorf("batch insert decisions: %w", err)
	}

	// Write decisions to Redis for the send worker.
	if err := ap.WriteDecisionsToRedis(ctx, ac.CampaignID, decisions); err != nil {
		return 0, fmt.Errorf("write decisions to redis: %w", err)
	}

	// Update recipient count on the agent-campaign row.
	_, err = ap.db.ExecContext(ctx,
		`UPDATE mailing_agent_campaigns SET recipient_count = $1 WHERE id = $2`,
		len(decisions), ac.ID,
	)
	if err != nil {
		return 0, fmt.Errorf("update recipient count: %w", err)
	}

	return len(decisions), nil
}

// ---------------------------------------------------------------------------
// 3. classifyRecipient — intelligence core
// ---------------------------------------------------------------------------

// classifyRecipient evaluates a single inbox profile against agent knowledge
// and returns a send decision.
func classifyRecipient(p *inboxProfile, agentKnowledge json.RawMessage) AgentDecision {
	d := AgentDecision{
		Reasoning: make(map[string]interface{}),
	}

	// ---- Suppression checks (most restrictive first) ----

	if p.SendSuspendedUntil.Valid && p.SendSuspendedUntil.Time.After(time.Now()) {
		d.Classification = "suppress"
		d.Reasoning["rule"] = "send_suspended"
		d.Reasoning["suspended_until"] = p.SendSuspendedUntil.Time.Format(time.RFC3339)
		return finalizeDecision(d, p)
	}

	if p.InboxHealth.Valid && p.InboxHealth.String == "full" {
		d.Classification = "suppress"
		d.Reasoning["rule"] = "inbox_full"
		return finalizeDecision(d, p)
	}

	if p.ConsecutiveBounces >= 5 {
		d.Classification = "suppress"
		d.Reasoning["rule"] = "consecutive_bounces"
		d.Reasoning["bounces"] = p.ConsecutiveBounces
		return finalizeDecision(d, p)
	}

	if p.TotalComplaints > 0 {
		// Check if any complaint is recent (within 90 days).
		// We treat a total_complaints > 0 as recent when we have no finer timestamp.
		d.Classification = "suppress"
		d.Reasoning["rule"] = "complaints"
		d.Reasoning["total_complaints"] = p.TotalComplaints
		return finalizeDecision(d, p)
	}

	if p.EngagementScore < 0.05 && p.TotalSends > 20 {
		d.Classification = "suppress"
		d.Reasoning["rule"] = "chronic_non_opener"
		d.Reasoning["engagement_score"] = p.EngagementScore
		d.Reasoning["total_sends"] = p.TotalSends
		return finalizeDecision(d, p)
	}

	// ---- Defer checks ----

	if p.InboxHealth.Valid && p.InboxHealth.String == "degraded" {
		d.Classification = "defer"
		d.Reasoning["rule"] = "inbox_degraded"
		return finalizeDecision(d, p)
	}

	if p.EngagementScore < 0.15 && p.TotalSends > 10 {
		d.Classification = "defer"
		d.Reasoning["rule"] = "low_engagement"
		d.Reasoning["engagement_score"] = p.EngagementScore
		d.Reasoning["total_sends"] = p.TotalSends
		return finalizeDecision(d, p)
	}

	if isSeasonallyDormant(p.MonthlyEngagement) {
		d.Classification = "defer"
		d.Reasoning["rule"] = "seasonally_dormant"
		return finalizeDecision(d, p)
	}

	if !p.LastOpenAt.Valid && p.TotalSends > 5 {
		d.Classification = "defer"
		d.Reasoning["rule"] = "never_opened"
		d.Reasoning["total_sends"] = p.TotalSends
		return finalizeDecision(d, p)
	}

	// ---- Send-later check ----

	currentHour := time.Now().UTC().Hour()
	hourDiff := absDiffHours(currentHour, p.OptimalSendHour)
	if hourDiff > 2 {
		d.Classification = "send_later"
		d.Reasoning["rule"] = "outside_optimal_window"
		d.Reasoning["current_hour"] = currentHour
		d.Reasoning["optimal_send_hour"] = p.OptimalSendHour
		return finalizeDecision(d, p)
	}

	// ---- Default: send now ----

	d.Classification = "send_now"
	d.Reasoning["rule"] = "default_send"
	return finalizeDecision(d, p)
}

// finalizeDecision fills in content strategy, priority, and optimal hour.
func finalizeDecision(d AgentDecision, p *inboxProfile) AgentDecision {
	d.ContentStrategy = chooseContentStrategy(p)
	d.Priority = computePriority(p)
	d.OptimalSendHour = p.OptimalSendHour
	return d
}

// chooseContentStrategy determines the best content variant for a recipient.
func chooseContentStrategy(p *inboxProfile) string {
	// Determine text vs image preference.
	variant := "image" // default
	if p.PrefTextScore > p.PrefImageScore+0.15 {
		variant = "text"
	} else if p.PrefImageScore > p.PrefTextScore+0.15 {
		variant = "image"
	}

	// Determine personalized vs generic.
	personalization := "generic"
	if p.PrefPersonalizedScore > 0.5 {
		personalization = "personalized"
	}

	return variant + "_" + personalization
}

// computePriority scores a recipient 0-100 for send ordering.
func computePriority(p *inboxProfile) int {
	priority := p.EngagementScore * 60.0

	if p.RevenueTotal > 0 {
		priority += 20
	}

	if p.EngagementTrend.Valid {
		switch p.EngagementTrend.String {
		case "improving":
			priority += 10
		case "declining":
			priority -= 10
		}
	}

	totalSends := float64(p.TotalSends)
	if totalSends < 1 {
		totalSends = 1
	}
	openRate := float64(p.TotalOpens) / totalSends
	if openRate > 0.3 {
		priority += 10
	}

	// Clamp 0-100
	if priority < 0 {
		priority = 0
	}
	if priority > 100 {
		priority = 100
	}
	return int(math.Round(priority))
}

// ---------------------------------------------------------------------------
// 4. isSeasonallyDormant
// ---------------------------------------------------------------------------

// isSeasonallyDormant checks if the recipient historically has zero opens in
// the current calendar month across 2+ years of data.
func isSeasonallyDormant(monthlyEngagement json.RawMessage) bool {
	if len(monthlyEngagement) == 0 {
		return false
	}

	// Expected structure: {"2024-02": {"opens": 0, ...}, "2023-02": {"opens": 3}, ...}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(monthlyEngagement, &data); err != nil {
		return false
	}
	if len(data) == 0 {
		return false
	}

	currentMonth := fmt.Sprintf("%02d", int(time.Now().Month()))

	type monthEntry struct {
		Opens int `json:"opens"`
	}

	matchingYears := 0
	zeroYears := 0

	for key, raw := range data {
		// Keys are "YYYY-MM".
		parts := strings.SplitN(key, "-", 2)
		if len(parts) != 2 {
			continue
		}
		if parts[1] != currentMonth {
			continue
		}
		matchingYears++

		var entry monthEntry
		if err := json.Unmarshal(raw, &entry); err != nil {
			continue
		}
		if entry.Opens == 0 {
			zeroYears++
		}
	}

	// Dormant if 2+ years of data for this month all show zero opens.
	return matchingYears >= 2 && zeroYears == matchingYears
}

// ---------------------------------------------------------------------------
// 5. WriteDecisionsToRedis
// ---------------------------------------------------------------------------

// WriteDecisionsToRedis pipelines all decisions into Redis with a 24-hour TTL.
// It also writes a summary key with per-classification counts.
func (ap *AgentPreprocessor) WriteDecisionsToRedis(ctx context.Context, campaignID uuid.UUID, decisions []AgentDecision) error {
	if len(decisions) == 0 {
		return nil
	}

	ttl := 24 * time.Hour
	pipe := ap.redis.Pipeline()

	summary := campaignSummary{}

	for i := range decisions {
		d := &decisions[i]

		key := fmt.Sprintf("agent:decisions:%s:%s", campaignID, d.EmailHash)
		payload := decisionRedisPayload{
			Classification:  d.Classification,
			ContentStrategy: d.ContentStrategy,
			Priority:        d.Priority,
			OptimalSendHour: d.OptimalSendHour,
		}
		data, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("marshal decision for %s: %w", d.EmailHash, err)
		}
		pipe.Set(ctx, key, data, ttl)

		switch d.Classification {
		case "send_now":
			summary.SendNow++
		case "send_later":
			summary.SendLater++
		case "defer":
			summary.Defer++
		case "suppress":
			summary.Suppress++
		}
		summary.Total++
	}

	// Write campaign-level summary.
	summaryKey := fmt.Sprintf("agent:campaign:%s:summary", campaignID)
	summaryData, err := json.Marshal(summary)
	if err != nil {
		return fmt.Errorf("marshal campaign summary: %w", err)
	}
	pipe.Set(ctx, summaryKey, summaryData, ttl)

	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("redis pipeline exec: %w", err)
	}

	return nil
}

// ---------------------------------------------------------------------------
// 6. GetDecisionFromRedis
// ---------------------------------------------------------------------------

// GetDecisionFromRedis fetches a single recipient's decision from Redis.
// Returns nil when no decision exists (non-agent campaign or expired).
func (ap *AgentPreprocessor) GetDecisionFromRedis(ctx context.Context, campaignID uuid.UUID, emailHash string) (*AgentDecision, error) {
	key := fmt.Sprintf("agent:decisions:%s:%s", campaignID, emailHash)

	data, err := ap.redis.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("redis GET %s: %w", key, err)
	}

	var payload decisionRedisPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("unmarshal decision %s: %w", key, err)
	}

	return &AgentDecision{
		EmailHash:       emailHash,
		Classification:  payload.Classification,
		ContentStrategy: payload.ContentStrategy,
		Priority:        payload.Priority,
		OptimalSendHour: payload.OptimalSendHour,
	}, nil
}

// ---------------------------------------------------------------------------
// 7. CleanupCampaignDecisions
// ---------------------------------------------------------------------------

// CleanupCampaignDecisions removes all Redis keys for a completed campaign
// using SCAN + DEL to avoid blocking the server.
func (ap *AgentPreprocessor) CleanupCampaignDecisions(ctx context.Context, campaignID uuid.UUID) error {
	pattern := fmt.Sprintf("agent:decisions:%s:*", campaignID)
	deleted := 0

	iter := ap.redis.Scan(ctx, 0, pattern, 200).Iterator()
	pipe := ap.redis.Pipeline()
	batch := 0

	for iter.Next(ctx) {
		pipe.Del(ctx, iter.Val())
		batch++
		deleted++

		if batch >= 500 {
			if _, err := pipe.Exec(ctx); err != nil {
				return fmt.Errorf("redis cleanup pipeline exec: %w", err)
			}
			pipe = ap.redis.Pipeline()
			batch = 0
		}
	}
	if err := iter.Err(); err != nil {
		return fmt.Errorf("redis SCAN %s: %w", pattern, err)
	}

	// Flush remaining deletes.
	if batch > 0 {
		if _, err := pipe.Exec(ctx); err != nil {
			return fmt.Errorf("redis cleanup pipeline exec (final): %w", err)
		}
	}

	// Also remove the summary key.
	summaryKey := fmt.Sprintf("agent:campaign:%s:summary", campaignID)
	ap.redis.Del(ctx, summaryKey)

	log.Printf("[AgentPreprocessor] Cleaned up %d decision keys for campaign %s", deleted, campaignID)
	return nil
}

// ---------------------------------------------------------------------------
// Internal helpers — DB loaders
// ---------------------------------------------------------------------------

func (ap *AgentPreprocessor) loadAgentCampaigns(ctx context.Context, campaignID uuid.UUID) ([]agentCampaign, error) {
	rows, err := ap.db.QueryContext(ctx,
		`SELECT id, agent_id, campaign_id, recipient_count, status, send_window
		 FROM mailing_agent_campaigns
		 WHERE campaign_id = $1`,
		campaignID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []agentCampaign
	for rows.Next() {
		var ac agentCampaign
		if err := rows.Scan(&ac.ID, &ac.AgentID, &ac.CampaignID, &ac.RecipientCount, &ac.Status, &ac.SendWindow); err != nil {
			return nil, fmt.Errorf("scan agent_campaign: %w", err)
		}
		out = append(out, ac)
	}
	return out, rows.Err()
}

func (ap *AgentPreprocessor) loadAgent(ctx context.Context, agentID uuid.UUID) (*ispAgent, error) {
	a := &ispAgent{}
	err := ap.db.QueryRowContext(ctx,
		`SELECT id, isp, domain, status, config, knowledge
		 FROM mailing_isp_agents
		 WHERE id = $1`,
		agentID,
	).Scan(&a.ID, &a.ISP, &a.Domain, &a.Status, &a.Config, &a.Knowledge)
	if err != nil {
		return nil, err
	}
	return a, nil
}

func (ap *AgentPreprocessor) loadInboxProfiles(ctx context.Context, domain string) ([]inboxProfile, error) {
	rows, err := ap.db.QueryContext(ctx,
		`SELECT email_hash, engagement_score, engagement_trend, total_sends, total_opens,
		        total_clicks, total_bounces, total_complaints, optimal_send_hour,
		        pref_text_score, pref_image_score, pref_personalized_score,
		        inbox_health, send_suspended_until, monthly_engagement,
		        revenue_total, consecutive_bounces, last_open_at, last_click_at
		 FROM mailing_inbox_profiles
		 WHERE domain = $1
		 ORDER BY engagement_score DESC`,
		domain,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []inboxProfile
	for rows.Next() {
		var p inboxProfile
		if err := rows.Scan(
			&p.EmailHash, &p.EngagementScore, &p.EngagementTrend,
			&p.TotalSends, &p.TotalOpens, &p.TotalClicks, &p.TotalBounces,
			&p.TotalComplaints, &p.OptimalSendHour,
			&p.PrefTextScore, &p.PrefImageScore, &p.PrefPersonalizedScore,
			&p.InboxHealth, &p.SendSuspendedUntil, &p.MonthlyEngagement,
			&p.RevenueTotal, &p.ConsecutiveBounces, &p.LastOpenAt, &p.LastClickAt,
		); err != nil {
			return nil, fmt.Errorf("scan inbox_profile: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Internal helpers — batch insert
// ---------------------------------------------------------------------------

// batchInsertDecisions writes decisions to mailing_agent_send_decisions in
// batches of 500 using a multi-row VALUES builder.
func (ap *AgentPreprocessor) batchInsertDecisions(ctx context.Context, agentID, campaignID uuid.UUID, decisions []AgentDecision) error {
	const batchSize = 500

	for start := 0; start < len(decisions); start += batchSize {
		end := start + batchSize
		if end > len(decisions) {
			end = len(decisions)
		}
		batch := decisions[start:end]

		if err := ap.insertDecisionBatch(ctx, agentID, campaignID, batch); err != nil {
			return fmt.Errorf("batch %d-%d: %w", start, end, err)
		}
	}
	return nil
}

func (ap *AgentPreprocessor) insertDecisionBatch(ctx context.Context, agentID, campaignID uuid.UUID, batch []AgentDecision) error {
	if len(batch) == 0 {
		return nil
	}

	var sb strings.Builder
	sb.WriteString(`INSERT INTO mailing_agent_send_decisions
		(id, agent_id, campaign_id, email_hash, classification, content_strategy, optimal_send_at, priority, reasoning, executed)
		VALUES `)

	args := make([]interface{}, 0, len(batch)*8)
	argIdx := 1

	for i, d := range batch {
		if i > 0 {
			sb.WriteString(", ")
		}

		reasoningJSON, err := json.Marshal(d.Reasoning)
		if err != nil {
			reasoningJSON = []byte("{}")
		}

		// Build optimal_send_at from today's date + optimal hour.
		optimalAt := todayAtHourUTC(d.OptimalSendHour)

		sb.WriteString(fmt.Sprintf("($%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, false)",
			argIdx, argIdx+1, argIdx+2, argIdx+3, argIdx+4, argIdx+5, argIdx+6, argIdx+7, argIdx+8))

		args = append(args,
			uuid.New(),         // id
			agentID,            // agent_id
			campaignID,         // campaign_id
			d.EmailHash,        // email_hash
			d.Classification,   // classification
			d.ContentStrategy,  // content_strategy
			optimalAt,          // optimal_send_at
			d.Priority,         // priority
			reasoningJSON,      // reasoning
		)
		argIdx += 9
	}

	_, err := ap.db.ExecContext(ctx, sb.String(), args...)
	return err
}

// ---------------------------------------------------------------------------
// Internal helpers — utility
// ---------------------------------------------------------------------------

// absDiffHours returns the minimum circular distance between two hours (0-23).
func absDiffHours(a, b int) int {
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	if diff > 12 {
		diff = 24 - diff
	}
	return diff
}

// todayAtHourUTC returns today's date with the given hour in UTC.
func todayAtHourUTC(hour int) time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, time.UTC)
}
