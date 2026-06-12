package api

// Domain Agent chat — the conversational interface embedded in the Domain
// Agent screen. Gives the operator a full-power scheduling copilot in the
// portal: it can read scorecards/briefings, draft and edit per-domain plans,
// clone and deploy campaigns, approve compiled plans (deploying their
// payloads), monitor live state, and emergency-stop — the same surface the
// Domain Agent tab + Campaign Copilot expose, driven by chat.
//
// Architecture mirrors CreativeAgent: the EDITH LLM adapter (Anthropic-first)
// + a tool-calling loop, with its own conversation tables so EDITH's list
// stays clean. Tools come from two places:
//   - the Campaign Copilot's full toolset, dispatched verbatim through
//     CampaignCopilot.executeCopilotTool (clone/deploy/stop/segments/etc.)
//   - da_* tools implemented here against the Domain Agent plan lifecycle
//     (scorecard → briefing → draft plan → slots → approve-to-deploy)
//
// Safety contract: destructive/irreversible tools (deploy_campaign,
// da_approve_plan, emergency_stop) are guarded by the system prompt's
// explicit-confirmation rule, and da_approve_plan additionally requires the
// operator-typed sending domain to match the plan — the same typed-domain
// gate the ApproveDeploySection UI enforces.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ignite/sparkpost-monitor/internal/config"
	"github.com/ignite/sparkpost-monitor/internal/domainagent"
	"github.com/ignite/sparkpost-monitor/internal/engine"
)

type DomainAgentChat struct {
	db      *sql.DB
	llm     *EmailMarketingAgent // adapter only: callClaude/callAgentOpenAI + key/model resolution
	agent   *DomainAgentAPI
	copilot *CampaignCopilot
}

func NewDomainAgentChat(db *sql.DB, cfg config.OpenAIConfig, agent *DomainAgentAPI, copilot *CampaignCopilot) *DomainAgentChat {
	c := &DomainAgentChat{
		db:      db,
		llm:     NewEmailMarketingAgent(db, cfg, nil, nil),
		agent:   agent,
		copilot: copilot,
	}
	c.ensureTables()
	return c
}

func (c *DomainAgentChat) ensureTables() {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS domain_agent_chat_conversations (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			organization_id UUID NOT NULL,
			title TEXT NOT NULL DEFAULT '',
			message_count INTEGER NOT NULL DEFAULT 0,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS domain_agent_chat_messages (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			conversation_id UUID NOT NULL REFERENCES domain_agent_chat_conversations(id) ON DELETE CASCADE,
			role VARCHAR(20) NOT NULL,
			content TEXT NOT NULL DEFAULT '',
			tool_calls JSONB,
			tool_call_id TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_da_chat_messages_convo ON domain_agent_chat_messages(conversation_id, created_at)`,
	}
	for _, s := range stmts {
		if _, err := c.db.Exec(s); err != nil {
			log.Printf("[DomainAgentChat] ensure tables: %v", err)
		}
	}
}

const domainAgentChatSystemPrompt = `You are the Domain Agent — the operator's conversational scheduling copilot for the Project Jarvis email platform. You manage per-sending-domain campaign scheduling end to end: reading delivery scorecards, drafting daily plans, adjusting slots, cloning and deploying campaigns, approving compiled plans, and monitoring live sends.

The operator is an expert. Be direct, concise, numbers-first. No pleasantries, no disclaimers.

How the platform works:
- Sending domains are em.<brand> (e.g. em.discountblog.com). Each has a per-ISP scorecard (sends, human vs machine opens, clicks, hard vs soft bounces).
- The plan lifecycle is: draft (briefing + slots) → compiled (payloads attached by the operator-side CLI "python -m agents.domainagent compile") → approved → deployed. You can generate/edit plans and approve+deploy COMPILED plans. You cannot compile payloads server-side — if a plan is stuck in draft and the operator wants it deployable, tell them to run the compile CLI, or offer to clone a recent proven campaign instead (clone_campaign → adjust → deploy_campaign), which you CAN do entirely yourself.
- Bounces are ALWAYS reported split: hard (permanent, reputation risk) vs soft (transient). Never combine them.
- Opens include Apple MPP / scanner machine traffic; the scorecard splits human vs machine opens — prefer human metrics when judging engagement.
- Campaign deploys go through the PMTA wave pipeline: scheduled → preparing → finalizing_audience → sending. A campaign stuck in scheduled >10min or finalized with 0 recipients is a problem worth flagging.

Confirmation rules (HARD requirements):
- deploy_campaign, da_approve_plan, and emergency_stop are irreversible. NEVER call them unless the operator has explicitly confirmed THAT SPECIFIC action in their latest message (e.g. "yes, deploy it", "approve em.discountblog.com", "stop it now"). Proposing something and the operator replying "ok" to an unrelated point does NOT count.
- Before asking for confirmation, show a compact summary of exactly what will happen (campaign name(s), audience source, schedule, domain).
- da_approve_plan requires confirm_domain to be the exact sending domain — ask the operator to type it if they haven't.
- Everything else (reads, drafting plans, editing slots, cloning to DRAFT, estimates) you do freely without asking.

Style: answer in tight markdown. Lead with the number or the action result. When you take actions, state what you did and the resulting IDs/statuses.`

// domainAgentChatTools merges the Domain Agent lifecycle tools with the full
// Campaign Copilot toolset (converted to the shared agentToolDef shape).
func domainAgentChatTools() []agentToolDef {
	obj := func(props map[string]interface{}, required ...string) map[string]interface{} {
		m := map[string]interface{}{"type": "object", "properties": props}
		if len(required) > 0 {
			m["required"] = required
		}
		return m
	}
	str := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "string", "description": desc}
	}
	num := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "number", "description": desc}
	}

	tools := []agentToolDef{
		{Type: "function", Function: agentToolFuncDef{
			Name:        "da_list_domains",
			Description: "List active sending domains with 7-day rollups (sends, human opens, hard/soft bounces) and whether a plan exists today. Start here when the operator references a domain loosely.",
			Parameters:  obj(map[string]interface{}{}),
		}},
		{Type: "function", Function: agentToolFuncDef{
			Name:        "da_get_scorecard",
			Description: "Per-ISP scorecard rows for one sending domain (sends, human/machine opens, clicks, hard/soft bounces, complaints) over a lookback window.",
			Parameters: obj(map[string]interface{}{
				"domain": str("sending domain, e.g. em.discountblog.com"),
				"days":   num("lookback days (default 7, max 30)"),
			}, "domain"),
		}},
		{Type: "function", Function: agentToolFuncDef{
			Name:        "da_get_briefing",
			Description: "Deterministic per-domain briefing: posture per ISP, recommended quotas/cautions, derived from the scorecard. Use before drafting or judging a plan.",
			Parameters: obj(map[string]interface{}{
				"domain": str("sending domain"),
				"days":   num("lookback days (default 7)"),
			}, "domain"),
		}},
		{Type: "function", Function: agentToolFuncDef{
			Name:        "da_generate_plan",
			Description: "Create (or refresh the briefing of) the domain's draft plan for a date. Operator-edited slots survive a refresh.",
			Parameters: obj(map[string]interface{}{
				"domain": str("sending domain"),
				"date":   str("YYYY-MM-DD (default today UTC)"),
			}, "domain"),
		}},
		{Type: "function", Function: agentToolFuncDef{
			Name:        "da_get_plan",
			Description: "Fetch a plan by id, or by domain (+optional date, default today). Returns status, briefing, slots, payload count, deploy results.",
			Parameters: obj(map[string]interface{}{
				"plan_id": str("plan UUID (preferred when known)"),
				"domain":  str("sending domain (when plan_id unknown)"),
				"date":    str("YYYY-MM-DD (default today)"),
			}),
		}},
		{Type: "function", Function: agentToolFuncDef{
			Name:        "da_update_slots",
			Description: "Replace the slots JSON on a draft/compiled plan. Pass the COMPLETE slots array (it overwrites). Fetch the plan first, modify, then write back.",
			Parameters: obj(map[string]interface{}{
				"plan_id": str("plan UUID"),
				"slots":   map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "object"}, "description": "complete slots array"},
			}, "plan_id", "slots"),
		}},
		{Type: "function", Function: agentToolFuncDef{
			Name:        "da_approve_plan",
			Description: "IRREVERSIBLE: approve a COMPILED plan and deploy every attached payload through the PMTA pipeline. Requires the operator to have typed the exact sending domain as confirmation in chat.",
			Parameters: obj(map[string]interface{}{
				"plan_id":        str("plan UUID"),
				"confirm_domain": str("the exact sending domain the operator typed to confirm"),
			}, "plan_id", "confirm_domain"),
		}},
		{Type: "function", Function: agentToolFuncDef{
			Name:        "da_plan_status",
			Description: "Live state of the campaigns a deployed plan created (status, recipients, sent counts).",
			Parameters:  obj(map[string]interface{}{"plan_id": str("plan UUID")}, "plan_id"),
		}},
		{Type: "function", Function: agentToolFuncDef{
			Name:        "da_upcoming",
			Description: "Campaigns scheduled in the next 48h, optionally filtered to one sending domain — the forward-looking board.",
			Parameters:  obj(map[string]interface{}{"domain": str("optional sending domain filter")}),
		}},
	}

	for _, t := range getCopilotTools() {
		tools = append(tools, agentToolDef{
			Type: t.Type,
			Function: agentToolFuncDef{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				Parameters:  t.Function.Parameters,
			},
		})
	}
	return tools
}

// ---------------------------------------------------------------------------
// Chat endpoint
// ---------------------------------------------------------------------------

type domainAgentChatRequest struct {
	Message        string `json:"message"`
	ConversationID string `json:"conversation_id,omitempty"`
}

func (c *DomainAgentChat) RegisterRoutes(r chi.Router) {
	r.Route("/domain-agent/chat", func(cr chi.Router) {
		cr.Post("/", c.HandleChat)
		cr.Get("/conversations", c.HandleListConversations)
		cr.Get("/conversations/{id}", c.HandleGetConversation)
	})
}

func (c *DomainAgentChat) HandleChat(w http.ResponseWriter, r *http.Request) {
	if c.llm.anthropicKey == "" && c.llm.openAIKey == "" {
		respondJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "AI not configured (ANTHROPIC_API_KEY / OpenAI)"})
		return
	}
	var req domainAgentChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Message) == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "message is required"})
		return
	}
	orgID := getOrgID(r)
	ctx := r.Context()

	convoID := req.ConversationID
	if convoID == "" {
		title := req.Message
		if len(title) > 80 {
			title = title[:80]
		}
		if err := c.db.QueryRowContext(ctx,
			`INSERT INTO domain_agent_chat_conversations (organization_id, title) VALUES ($1,$2) RETURNING id`,
			orgID, title).Scan(&convoID); err != nil {
			respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "conversation create: " + err.Error()})
			return
		}
	}

	history := c.loadHistory(ctx, convoID, 60)
	messages := []agentOpenAIMsg{{Role: "system", Content: domainAgentChatSystemPrompt}}
	messages = append(messages, history...)
	messages = append(messages, agentOpenAIMsg{Role: "user", Content: req.Message})
	c.persist(ctx, convoID, "user", req.Message, nil, "")

	tools := domainAgentChatTools()
	var actionsTaken []string
	var assistantContent string

	for i := 0; i < 16; i++ {
		var resp *agentOpenAIResp
		var err error
		if c.llm.useAnthropic {
			resp, err = c.llm.callClaude(ctx, domainAgentChatSystemPrompt, messages, tools)
			// Anthropic-side failures (billing, rate limits, outages) fall
			// back to OpenAI at call time so the operator's chat keeps
			// working — observed live 2026-06-12 ("credit balance too low").
			if err != nil && c.llm.openAIKey != "" {
				log.Printf("[DomainAgentChat] Claude failed (%v) — falling back to OpenAI", err)
				resp, err = c.llm.callAgentOpenAI(ctx, agentOpenAIReq{
					Model: "gpt-4.1", Messages: messages, Tools: tools,
					Temperature: 0.3, MaxCompletionTokens: 8000,
				})
			}
		} else {
			resp, err = c.llm.callAgentOpenAI(ctx, agentOpenAIReq{
				Model: c.llm.model, Messages: messages, Tools: tools,
				Temperature: 0.3, MaxCompletionTokens: 8000,
			})
		}
		if err != nil {
			log.Printf("[DomainAgentChat] LLM error: %v", err)
			respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "AI service error"})
			return
		}
		if len(resp.Choices) == 0 {
			respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "empty AI response"})
			return
		}
		choice := resp.Choices[0]

		if choice.FinishReason == "tool_calls" && len(choice.Message.ToolCalls) > 0 {
			tcJSON, _ := json.Marshal(choice.Message.ToolCalls)
			c.persist(ctx, convoID, "assistant", "", tcJSON, "")
			messages = append(messages, choice.Message)
			for _, tc := range choice.Message.ToolCalls {
				result, action := c.execute(ctx, orgID, tc.Function.Name, tc.Function.Arguments)
				if action != "" {
					actionsTaken = append(actionsTaken, action)
				}
				c.persist(ctx, convoID, "tool", result, nil, tc.ID)
				messages = append(messages, agentOpenAIMsg{Role: "tool", Content: result, ToolCallID: tc.ID})
			}
			continue
		}
		assistantContent = choice.Message.Content
		break
	}

	if assistantContent == "" {
		assistantContent = "I hit a processing limit — try rephrasing or splitting the request."
	}
	c.persist(ctx, convoID, "assistant", assistantContent, nil, "")
	c.db.ExecContext(ctx,
		`UPDATE domain_agent_chat_conversations SET updated_at = NOW(),
		 message_count = (SELECT COUNT(*) FROM domain_agent_chat_messages WHERE conversation_id = $1)
		 WHERE id = $1`, convoID)

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"response":        assistantContent,
		"conversation_id": convoID,
		"actions_taken":   actionsTaken,
	})
}

// ---------------------------------------------------------------------------
// Tool execution
// ---------------------------------------------------------------------------

func (c *DomainAgentChat) execute(ctx context.Context, orgID, name, rawArgs string) (result, action string) {
	if !strings.HasPrefix(name, "da_") {
		return c.copilot.executeCopilotTool(ctx, orgID, name, rawArgs)
	}

	var args map[string]interface{}
	if rawArgs != "" {
		_ = json.Unmarshal([]byte(rawArgs), &args)
	}
	argStr := func(k string) string {
		if v, ok := args[k].(string); ok {
			return strings.TrimSpace(v)
		}
		return ""
	}
	argInt := func(k string, def int) int {
		if v, ok := args[k].(float64); ok && v > 0 {
			return int(v)
		}
		return def
	}
	fail := func(msg string) (string, string) {
		out, _ := json.Marshal(map[string]string{"error": msg})
		return string(out), ""
	}
	ok := func(v interface{}) string {
		out, _ := json.Marshal(v)
		return string(out)
	}
	log.Printf("[DomainAgentChat] tool=%s args=%s", name, rawArgs)

	switch name {
	case "da_list_domains":
		return ok(c.toolListDomains(ctx, orgID)), ""

	case "da_get_scorecard":
		domain := canonicalSendingDomain(argStr("domain"))
		if domain == "" {
			return fail("domain is required")
		}
		days := argInt("days", 7)
		if days > 30 {
			days = 30
		}
		return ok(c.toolScorecard(ctx, orgID, domain, days)), ""

	case "da_get_briefing":
		domain := canonicalSendingDomain(argStr("domain"))
		if domain == "" {
			return fail("domain is required")
		}
		briefing, err := domainagent.GenerateBriefing(ctx, c.db, orgID, domain, argInt("days", 7))
		if err != nil {
			return fail(err.Error())
		}
		return ok(briefing), ""

	case "da_generate_plan":
		domain := canonicalSendingDomain(argStr("domain"))
		if domain == "" {
			return fail("domain is required")
		}
		planDate := time.Now().UTC()
		if d := argStr("date"); d != "" {
			parsed, err := time.Parse("2006-01-02", d)
			if err != nil {
				return fail("date must be YYYY-MM-DD")
			}
			planDate = parsed
		}
		briefing, err := domainagent.GenerateBriefing(ctx, c.db, orgID, domain, 7)
		if err != nil {
			return fail(err.Error())
		}
		briefingJSON, _ := json.Marshal(briefing)
		plan, err := c.agent.plans.GetByDomainDate(ctx, orgID, domain, planDate)
		if err != nil {
			plan, err = c.agent.plans.CreateDraft(ctx, orgID, domain, planDate, briefingJSON)
			if err != nil {
				return fail(err.Error())
			}
		} else if plan.Status == "draft" {
			if err := c.agent.plans.UpdateBriefing(ctx, orgID, plan.ID, briefingJSON); err != nil {
				return fail(err.Error())
			}
			plan, _ = c.agent.plans.Get(ctx, orgID, plan.ID)
		}
		return ok(planResponse(plan)), fmt.Sprintf("Generated plan for %s (%s)", domain, plan.Status)

	case "da_get_plan":
		if id := argStr("plan_id"); id != "" {
			plan, err := c.agent.plans.Get(ctx, orgID, id)
			if err != nil {
				return fail(err.Error())
			}
			return ok(planResponse(plan)), ""
		}
		domain := canonicalSendingDomain(argStr("domain"))
		if domain == "" {
			return fail("plan_id or domain is required")
		}
		planDate := time.Now().UTC()
		if d := argStr("date"); d != "" {
			parsed, err := time.Parse("2006-01-02", d)
			if err != nil {
				return fail("date must be YYYY-MM-DD")
			}
			planDate = parsed
		}
		plan, err := c.agent.plans.GetByDomainDate(ctx, orgID, domain, planDate)
		if err != nil {
			return fail(err.Error())
		}
		return ok(planResponse(plan)), ""

	case "da_update_slots":
		planID := argStr("plan_id")
		if planID == "" {
			return fail("plan_id is required")
		}
		slotsRaw, err := json.Marshal(args["slots"])
		if err != nil || string(slotsRaw) == "null" {
			return fail("slots array is required")
		}
		if err := c.agent.plans.UpdateSlots(ctx, orgID, planID, slotsRaw); err != nil {
			return fail(err.Error())
		}
		return ok(map[string]string{"status": "slots updated"}), "Updated plan slots"

	case "da_approve_plan":
		planID := argStr("plan_id")
		confirm := strings.ToLower(argStr("confirm_domain"))
		if planID == "" || confirm == "" {
			return fail("plan_id and confirm_domain are required")
		}
		plan, err := c.agent.plans.Get(ctx, orgID, planID)
		if err != nil {
			return fail(err.Error())
		}
		if confirm != strings.ToLower(plan.SendingDomain) {
			return fail(fmt.Sprintf("confirmation mismatch: plan is for %s — the operator must type that exact domain", plan.SendingDomain))
		}
		if plan.Status != "compiled" {
			return fail(fmt.Sprintf("plan must be compiled to approve (status=%s); compile via the operator CLI or clone+deploy instead", plan.Status))
		}
		var payloads []json.RawMessage
		if err := json.Unmarshal(plan.Payloads, &payloads); err != nil || len(payloads) == 0 {
			return fail("plan has no compiled payloads")
		}
		if err := c.agent.plans.Approve(ctx, orgID, planID, "domain-agent-chat"); err != nil {
			return fail(err.Error())
		}
		results := make([]domainAgentDeployResult, 0, len(payloads))
		allOK := true
		for i, raw := range payloads {
			var input engine.PMTACampaignInput
			if err := json.Unmarshal(raw, &input); err != nil {
				results = append(results, domainAgentDeployResult{Name: fmt.Sprintf("payload[%d]", i), Status: "failed", Error: "invalid PMTACampaignInput JSON: " + err.Error()})
				allOK = false
				continue
			}
			campaignID, status, err := c.agent.pmta.DeployFromInput(ctx, orgID, input)
			if err != nil {
				results = append(results, domainAgentDeployResult{Name: input.Name, Status: "failed", Error: err.Error()})
				allOK = false
				continue
			}
			results = append(results, domainAgentDeployResult{Name: input.Name, CampaignID: campaignID, Status: status})
		}
		finalStatus := "deployed"
		if !allOK {
			finalStatus = "failed"
		}
		resultsJSON, _ := json.Marshal(results)
		_ = c.agent.plans.SetDeployResults(ctx, orgID, planID, resultsJSON, finalStatus)
		return ok(map[string]interface{}{"status": finalStatus, "results": results}),
			fmt.Sprintf("Approved + deployed plan %s (%s): %s", planID[:8], plan.SendingDomain, finalStatus)

	case "da_plan_status":
		planID := argStr("plan_id")
		if planID == "" {
			return fail("plan_id is required")
		}
		return ok(c.toolPlanStatus(ctx, orgID, planID)), ""

	case "da_upcoming":
		return ok(c.toolUpcoming(ctx, orgID, canonicalSendingDomain(argStr("domain")))), ""
	}

	return fail("unknown tool: " + name)
}

func (c *DomainAgentChat) toolListDomains(ctx context.Context, orgID string) interface{} {
	rows, err := c.db.QueryContext(ctx, `
		SELECT s.sending_domain,
		       SUM(s.sends), SUM(s.human_opens), SUM(s.machine_opens),
		       SUM(s.human_clicks), SUM(s.hard_bounces), SUM(s.soft_bounces),
		       EXISTS (SELECT 1 FROM mailing_domain_agent_plans p
		               WHERE p.organization_id = s.organization_id
		                 AND p.sending_domain = s.sending_domain
		                 AND p.plan_date = CURRENT_DATE) AS has_plan_today
		FROM mailing_domain_agent_scorecard s
		WHERE s.organization_id = $1 AND s.day > CURRENT_DATE - 7
		GROUP BY s.organization_id, s.sending_domain
		ORDER BY SUM(s.sends) DESC
	`, orgID)
	if err != nil {
		return map[string]string{"error": err.Error()}
	}
	defer rows.Close()
	out := []map[string]interface{}{}
	for rows.Next() {
		var domain string
		var sends, hOpens, mOpens, clicks, hard, soft int
		var hasPlan bool
		if err := rows.Scan(&domain, &sends, &hOpens, &mOpens, &clicks, &hard, &soft, &hasPlan); err != nil {
			continue
		}
		out = append(out, map[string]interface{}{
			"sending_domain": domain, "sends_7d": sends,
			"human_opens_7d": hOpens, "machine_opens_7d": mOpens,
			"human_clicks_7d": clicks, "hard_bounces_7d": hard, "soft_bounces_7d": soft,
			"has_plan_today": hasPlan,
		})
	}
	return out
}

func (c *DomainAgentChat) toolScorecard(ctx context.Context, orgID, domain string, days int) interface{} {
	rows, err := c.db.QueryContext(ctx, `
		SELECT isp, SUM(sends), SUM(delivered), SUM(human_opens), SUM(machine_opens),
		       SUM(human_clicks), SUM(hard_bounces), SUM(soft_bounces), SUM(complaints)
		FROM mailing_domain_agent_scorecard
		WHERE organization_id = $1 AND sending_domain = $2 AND day > CURRENT_DATE - $3
		GROUP BY isp ORDER BY SUM(sends) DESC
	`, orgID, domain, days)
	if err != nil {
		return map[string]string{"error": err.Error()}
	}
	defer rows.Close()
	out := []map[string]interface{}{}
	for rows.Next() {
		var isp string
		var sends, delivered, hOpens, mOpens, clicks, hard, soft, complaints int
		if err := rows.Scan(&isp, &sends, &delivered, &hOpens, &mOpens, &clicks, &hard, &soft, &complaints); err != nil {
			continue
		}
		out = append(out, map[string]interface{}{
			"isp": isp, "sends": sends, "delivered": delivered,
			"human_opens": hOpens, "machine_opens": mOpens, "human_clicks": clicks,
			"hard_bounces": hard, "soft_bounces": soft, "complaints": complaints,
		})
	}
	return map[string]interface{}{"domain": domain, "days": days, "isps": out}
}

func (c *DomainAgentChat) toolPlanStatus(ctx context.Context, orgID, planID string) interface{} {
	plan, err := c.agent.plans.Get(ctx, orgID, planID)
	if err != nil {
		return map[string]string{"error": err.Error()}
	}
	var results []domainAgentDeployResult
	_ = json.Unmarshal(plan.DeployResults, &results)
	out := []map[string]interface{}{}
	for _, dr := range results {
		entry := map[string]interface{}{"name": dr.Name, "deploy_status": dr.Status}
		if dr.Error != "" {
			entry["error"] = dr.Error
		}
		if dr.CampaignID != "" {
			var status string
			var total, sent sql.NullInt64
			err := c.db.QueryRowContext(ctx, `
				SELECT status, total_recipients, sent_count FROM mailing_campaigns WHERE id = $1
			`, dr.CampaignID).Scan(&status, &total, &sent)
			if err == nil {
				entry["campaign_id"] = dr.CampaignID
				entry["live_status"] = status
				entry["total_recipients"] = total.Int64
				entry["sent_count"] = sent.Int64
			}
		}
		out = append(out, entry)
	}
	return map[string]interface{}{"plan_status": plan.Status, "campaigns": out}
}

func (c *DomainAgentChat) toolUpcoming(ctx context.Context, orgID, domain string) interface{} {
	rows, err := c.db.QueryContext(ctx, `
		SELECT c.id::text, c.name, c.status, c.scheduled_at,
		       COALESCE(c.total_recipients, 0), COALESCE(p.sending_domain, '')
		FROM mailing_campaigns c
		LEFT JOIN mailing_sending_profiles p ON p.id = c.sending_profile_id
		WHERE c.organization_id = $1
		  AND c.scheduled_at BETWEEN NOW() - INTERVAL '1 hour' AND NOW() + INTERVAL '48 hours'
		  AND c.status IN ('scheduled','preparing','finalizing_audience','sending')
		  AND ($2 = '' OR p.sending_domain = $2)
		ORDER BY c.scheduled_at ASC
		LIMIT 60
	`, orgID, domain)
	if err != nil {
		return map[string]string{"error": err.Error()}
	}
	defer rows.Close()
	out := []map[string]interface{}{}
	for rows.Next() {
		var id, name, status, dom string
		var scheduledAt sql.NullTime
		var recipients int
		if err := rows.Scan(&id, &name, &status, &scheduledAt, &recipients, &dom); err != nil {
			continue
		}
		sched := ""
		if scheduledAt.Valid {
			sched = scheduledAt.Time.Format(time.RFC3339)
		}
		out = append(out, map[string]interface{}{
			"campaign_id": id, "name": name, "status": status,
			"scheduled_at": sched, "total_recipients": recipients, "sending_domain": dom,
		})
	}
	return out
}

// ---------------------------------------------------------------------------
// Conversation persistence (mirrors CreativeAgent)
// ---------------------------------------------------------------------------

func (c *DomainAgentChat) persist(ctx context.Context, convoID, role, content string, toolCalls json.RawMessage, toolCallID string) {
	var tc interface{}
	if len(toolCalls) > 0 {
		tc = string(toolCalls)
	}
	if _, err := c.db.ExecContext(ctx, `
		INSERT INTO domain_agent_chat_messages (conversation_id, role, content, tool_calls, tool_call_id)
		VALUES ($1, $2, $3, $4, $5)
	`, convoID, role, content, tc, toolCallID); err != nil {
		log.Printf("[DomainAgentChat] persist message: %v", err)
	}
}

func (c *DomainAgentChat) loadHistory(ctx context.Context, convoID string, limit int) []agentOpenAIMsg {
	rows, err := c.db.QueryContext(ctx, `
		SELECT role, content, tool_calls, tool_call_id
		FROM (
			SELECT role, content, tool_calls, tool_call_id, created_at
			FROM domain_agent_chat_messages WHERE conversation_id = $1
			ORDER BY created_at DESC LIMIT $2
		) sub ORDER BY created_at ASC
	`, convoID, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []agentOpenAIMsg
	for rows.Next() {
		var role, content, toolCallID string
		var toolCalls sql.NullString
		if err := rows.Scan(&role, &content, &toolCalls, &toolCallID); err != nil {
			continue
		}
		msg := agentOpenAIMsg{Role: role, Content: content, ToolCallID: toolCallID}
		if toolCalls.Valid && toolCalls.String != "" {
			_ = json.Unmarshal([]byte(toolCalls.String), &msg.ToolCalls)
		}
		out = append(out, msg)
	}
	return out
}

func (c *DomainAgentChat) HandleListConversations(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	rows, err := c.db.QueryContext(r.Context(), `
		SELECT id, title, message_count, updated_at
		FROM domain_agent_chat_conversations
		WHERE organization_id = $1 ORDER BY updated_at DESC LIMIT 50
	`, orgID)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()
	out := []map[string]interface{}{}
	for rows.Next() {
		var id, title string
		var count int
		var updated time.Time
		if err := rows.Scan(&id, &title, &count, &updated); err != nil {
			continue
		}
		out = append(out, map[string]interface{}{
			"id": id, "title": title, "message_count": count, "updated_at": updated.Format(time.RFC3339),
		})
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"conversations": out})
}

func (c *DomainAgentChat) HandleGetConversation(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	convoID := chi.URLParam(r, "id")
	rows, err := c.db.QueryContext(r.Context(), `
		SELECT m.role, m.content, m.created_at
		FROM domain_agent_chat_messages m
		JOIN domain_agent_chat_conversations cv ON cv.id = m.conversation_id
		WHERE m.conversation_id = $1 AND cv.organization_id = $2
		  AND m.role IN ('user','assistant') AND m.content <> ''
		ORDER BY m.created_at ASC
	`, convoID, orgID)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()
	out := []map[string]interface{}{}
	for rows.Next() {
		var role, content string
		var created time.Time
		if err := rows.Scan(&role, &content, &created); err != nil {
			continue
		}
		out = append(out, map[string]interface{}{"role": role, "content": content, "created_at": created.Format(time.RFC3339)})
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"id": convoID, "messages": out})
}
