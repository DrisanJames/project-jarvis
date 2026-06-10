package api

// Creative agent — the conversational generator embedded in the Creative
// Studio screen. Replaces the operator's "ask the Cursor agent to make me a
// creative" loop: same request/response + tool-calling architecture as EDITH
// (marketing_agent.go), but with a small creative-scoped toolset that drives
// the ReviewForge engine sidecar through CreativeStudioService and persists
// results to the Content Library + mailing_creatives registry.
//
// Conversations live in their own tables (creative_agent_*) so EDITH's
// conversation list stays clean.

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
)

type CreativeAgent struct {
	db     *sql.DB
	studio *CreativeStudioService
	// llm is an EmailMarketingAgent constructed with nil tool deps — used
	// strictly for its callClaude/callAgentOpenAI adapters and key/model
	// resolution (Anthropic-first), never for EDITH tool execution.
	llm *EmailMarketingAgent
}

func NewCreativeAgent(db *sql.DB, cfg config.OpenAIConfig, studio *CreativeStudioService) *CreativeAgent {
	return &CreativeAgent{
		db:     db,
		studio: studio,
		llm:    NewEmailMarketingAgent(db, cfg, nil, nil),
	}
}

const creativeAgentSystemPrompt = `You are the Creative Studio agent for Project Jarvis, an email platform. You help the operator produce newsletter and solo-offer email creatives.

Ground rules:
- NEVER write raw email HTML yourself. Always render through the generate_newsletter / generate_solo tools — they run the production template engine (byte-identical to the operator's local ReviewForge).
- Site keys (sending brands): db, mh, qf, ht, bwp, fc, cp, hws, rru, tot, yih, mrd, ci, lpl, rb, wfy.
- Offer brands come from list_brands; use the exact brandKey. If the operator names an offer loosely ("warby", "NDR"), resolve it via list_brands first.
- Subjects: punchy, curiosity-driven, no spam-trigger words, < 65 chars. Preheaders complement (don't repeat) the subject, < 140 chars. When asked for copy ideas, give 3-5 options and ask which to apply.
- Money links are managed by the pipeline — never invent, modify, or ask about CTA URLs unless the operator brings them up.
- After generating, report the filename, subject, and that it's saved to the Content Library (Creative Studio folder) — the operator pulls it to send-day with "forge-pull".
- Be concise. The operator is an expert; skip pleasantries and disclaimers.`

func creativeAgentTools() []agentToolDef {
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
	boolean := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "boolean", "description": desc}
	}
	return []agentToolDef{
		{Type: "function", Function: agentToolFuncDef{
			Name:        "list_brands",
			Description: "List available offer brands (brandKey + name). Use to resolve loose brand references before generating.",
			Parameters:  obj(map[string]interface{}{"query": str("optional substring filter on key/name")}),
		}},
		{Type: "function", Function: agentToolFuncDef{
			Name:        "get_subject_pool",
			Description: "Get the standing subject/preheader pool for a site — useful grounding before proposing new copy.",
			Parameters:  obj(map[string]interface{}{"site_key": str("site key, e.g. db")}, "site_key"),
		}},
		{Type: "function", Function: agentToolFuncDef{
			Name:        "generate_newsletter",
			Description: "Render and SAVE newsletter creatives for one or more sites (sending brands) featuring an offer brand. Saves each to the Content Library + creative registry and returns the filenames. Use site_keys for multi-brand requests ('all brands' = all 16 site keys).",
			Parameters: obj(map[string]interface{}{
				"site_key": str("single site key (db, bwp, ...) — or use site_keys"),
				"site_keys": map[string]interface{}{
					"type": "array", "items": map[string]interface{}{"type": "string"},
					"description": "site keys to generate for, e.g. [\"db\",\"rru\"]",
				},
				"primary_brand_key":   str("offer brandKey from list_brands"),
				"secondary_brand_key": str("optional second offer for the compact card"),
				"subject_line":        str("optional subject override (else pool pick)"),
				"preheader":           str("optional preheader override"),
				"banner_url":          str("optional hero/banner image URL override (imagery swap)"),
				"logo_url":            str("optional logo image URL override"),
				"title":               str("optional headline/title override"),
				"cta_label":           str("optional CTA button text override"),
				"cta_url":             str("optional CTA URL override — only when the operator supplies it"),
				"refresh_content":     boolean("refetch live editorial feeds (default true)"),
				"name":                str("optional display name in the library"),
			}, "primary_brand_key"),
		}},
		{Type: "function", Function: agentToolFuncDef{
			Name:        "generate_solo",
			Description: "Render and SAVE a solo (full-page ad) creative: one hero creative image inside the site's header/footer shell.",
			Parameters: obj(map[string]interface{}{
				"site_key":          str("site key"),
				"primary_brand_key": str("offer brandKey"),
				"creative_url":      str("hosted hero image URL (required)"),
				"headline":          str("optional headline"),
				"subheadline":       str("optional subheadline"),
				"cta_label":         str("optional CTA button text"),
				"cta_url":           str("optional CTA URL override (default: brand promo URL)"),
				"below_mode":        str("review_card | full_review | none (default review_card)"),
				"subject_line":      str("optional subject"),
				"preheader":         str("optional preheader"),
			}, "site_key", "primary_brand_key", "creative_url"),
		}},
		{Type: "function", Function: agentToolFuncDef{
			Name:        "list_creatives",
			Description: "List recent creatives in the registry (offer, brand, filename, subject, source).",
			Parameters: obj(map[string]interface{}{
				"offer": str("optional offer substring filter"),
				"brand": str("optional brand code filter, e.g. DB"),
			}),
		}},
		{Type: "function", Function: agentToolFuncDef{
			Name:        "get_creative",
			Description: "Get one creative's subject, preheader and an HTML excerpt by registry id.",
			Parameters:  obj(map[string]interface{}{"id": str("creative registry id")}, "id"),
		}},
		{Type: "function", Function: agentToolFuncDef{
			Name:        "update_creative_copy",
			Description: "Update a creative's subject and/or preheader (registry + linked Content Library template). Body HTML is immutable here — regenerate instead.",
			Parameters: obj(map[string]interface{}{
				"id":        str("creative registry id"),
				"subject":   str("new subject (omit to keep)"),
				"preheader": str("new preheader (omit to keep)"),
			}, "id"),
		}},
	}
}

// ---------------------------------------------------------------------------
// Chat endpoint
// ---------------------------------------------------------------------------

type creativeChatRequest struct {
	Message        string `json:"message"`
	ConversationID string `json:"conversation_id,omitempty"`
}

func (ca *CreativeAgent) HandleChat(w http.ResponseWriter, r *http.Request) {
	if ca.llm.anthropicKey == "" && ca.llm.openAIKey == "" {
		respondJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "AI not configured (ANTHROPIC_API_KEY / OpenAI)"})
		return
	}
	var req creativeChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Message) == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "message is required"})
		return
	}
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	ctx := r.Context()

	convoID := req.ConversationID
	if convoID == "" {
		title := req.Message
		if len(title) > 80 {
			title = title[:80]
		}
		if err := ca.db.QueryRowContext(ctx,
			`INSERT INTO creative_agent_conversations (organization_id, title) VALUES ($1,$2) RETURNING id`,
			orgID, title).Scan(&convoID); err != nil {
			respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "conversation create: " + err.Error()})
			return
		}
	}

	history := ca.loadHistory(ctx, convoID, 40)
	messages := []agentOpenAIMsg{{Role: "system", Content: creativeAgentSystemPrompt}}
	messages = append(messages, history...)
	messages = append(messages, agentOpenAIMsg{Role: "user", Content: req.Message})
	ca.persist(ctx, convoID, "user", req.Message, nil, "")

	tools := creativeAgentTools()
	var actionsTaken []string
	var creativesCreated []map[string]string
	var assistantContent string

	for i := 0; i < 12; i++ {
		var resp *agentOpenAIResp
		var err error
		if ca.llm.useAnthropic {
			resp, err = ca.llm.callClaude(ctx, creativeAgentSystemPrompt, messages, tools)
		} else {
			resp, err = ca.llm.callAgentOpenAI(ctx, agentOpenAIReq{
				Model: ca.llm.model, Messages: messages, Tools: tools,
				Temperature: 0.4, MaxCompletionTokens: 8000,
			})
		}
		if err != nil {
			log.Printf("[CreativeAgent] LLM error: %v", err)
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
			ca.persist(ctx, convoID, "assistant", "", tcJSON, "")
			messages = append(messages, choice.Message)
			for _, tc := range choice.Message.ToolCalls {
				result, action, created := ca.execute(r, orgID.String(), tc.Function.Name, tc.Function.Arguments)
				if action != "" {
					actionsTaken = append(actionsTaken, action)
				}
				if created != nil {
					creativesCreated = append(creativesCreated, created)
				}
				ca.persist(ctx, convoID, "tool", result, nil, tc.ID)
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
	ca.persist(ctx, convoID, "assistant", assistantContent, nil, "")
	ca.db.ExecContext(ctx,
		`UPDATE creative_agent_conversations SET updated_at = NOW(),
		 message_count = (SELECT COUNT(*) FROM creative_agent_messages WHERE conversation_id = $1)
		 WHERE id = $1`, convoID)

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"response":          assistantContent,
		"conversation_id":   convoID,
		"actions_taken":     actionsTaken,
		"creatives_created": creativesCreated,
	})
}

// ---------------------------------------------------------------------------
// Tool execution
// ---------------------------------------------------------------------------

func (ca *CreativeAgent) execute(r *http.Request, orgID, name, rawArgs string) (result, action string, created map[string]string) {
	var args map[string]interface{}
	if rawArgs != "" {
		if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
			return "ERROR: bad tool arguments: " + err.Error(), "", nil
		}
	}
	getS := func(k string) string {
		if v, ok := args[k].(string); ok {
			return strings.TrimSpace(v)
		}
		return ""
	}

	switch name {
	case "list_brands":
		body, err := ca.studio.engineGET("/api/brands", 10*time.Second)
		if err != nil {
			return "ERROR: engine unreachable: " + err.Error(), "", nil
		}
		var parsed struct {
			Brands []studioBrand `json:"brands"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			return "ERROR: " + err.Error(), "", nil
		}
		q := strings.ToLower(getS("query"))
		var lines []string
		for _, b := range parsed.Brands {
			if q != "" && !strings.Contains(strings.ToLower(b.BrandKey+b.Name), q) {
				continue
			}
			lines = append(lines, fmt.Sprintf("%s | %s", b.BrandKey, b.Name))
			if len(lines) >= 60 {
				break
			}
		}
		if len(lines) == 0 {
			return "no brands matched", "", nil
		}
		return strings.Join(lines, "\n"), "", nil

	case "get_subject_pool":
		body, err := ca.studio.engineGET("/api/subject-lines", 10*time.Second)
		if err != nil {
			return "ERROR: engine unreachable: " + err.Error(), "", nil
		}
		var pools map[string][]struct {
			Subject   string `json:"subject"`
			Preheader string `json:"preheader"`
		}
		if err := json.Unmarshal(body, &pools); err != nil {
			return "ERROR: " + err.Error(), "", nil
		}
		pool, ok := pools[strings.ToLower(getS("site_key"))]
		if !ok {
			return "no pool for site " + getS("site_key"), "", nil
		}
		var lines []string
		for _, c := range pool {
			lines = append(lines, fmt.Sprintf("subj: %s | preh: %s", c.Subject, c.Preheader))
		}
		return strings.Join(lines, "\n"), "", nil

	case "generate_newsletter", "generate_solo":
		req := StudioGenerateRequest{
			SiteKey:           getS("site_key"),
			PrimaryBrandKey:   getS("primary_brand_key"),
			SecondaryBrandKey: getS("secondary_brand_key"),
			SubjectLine:       getS("subject_line"),
			Preheader:         getS("preheader"),
			RefreshContent:    true,
			Save:              true,
			Name:              getS("name"),
		}
		if v, ok := args["refresh_content"].(bool); ok {
			req.RefreshContent = v
		}
		overrides := map[string]interface{}{}
		if v := getS("banner_url"); v != "" {
			overrides["bannerUrl"] = v
		}
		if v := getS("logo_url"); v != "" {
			overrides["logoUrl"] = v
		}
		if v := getS("title"); v != "" {
			overrides["title"] = v
		}
		if v := getS("cta_label"); v != "" {
			overrides["ctaLabel"] = v
		}
		if v := getS("cta_url"); v != "" {
			overrides["ctaUrl"] = v
		}
		if len(overrides) > 0 {
			req.PrimaryOverrides, _ = json.Marshal(overrides)
		}
		if name == "generate_solo" {
			req.Mode = "solo"
			solo := map[string]interface{}{"creativeUrl": getS("creative_url")}
			if v := getS("headline"); v != "" {
				solo["headline"] = v
			}
			if v := getS("subheadline"); v != "" {
				solo["subheadline"] = v
			}
			if v := getS("cta_label"); v != "" {
				solo["ctaLabel"] = v
			}
			if v := getS("cta_url"); v != "" {
				solo["ctaUrl"] = v
			}
			below := getS("below_mode")
			if below == "" {
				below = "review_card"
			}
			solo["belowMode"] = below
			req.Solo, _ = json.Marshal(solo)
		}

		sites := []string{}
		if raw, ok := args["site_keys"].([]interface{}); ok {
			for _, v := range raw {
				if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
					sites = append(sites, strings.TrimSpace(s))
				}
			}
		}
		if len(sites) == 0 {
			if req.SiteKey == "" {
				return "ERROR: site_key or site_keys required", "", nil
			}
			sites = []string{req.SiteKey}
		}

		var lines []string
		okCount := 0
		for _, site := range sites {
			perSite := req
			perSite.SiteKey = site
			res, err := ca.studio.Generate(r, perSite)
			if err != nil {
				lines = append(lines, fmt.Sprintf("%s: ERROR %s", site, err.Error()))
				continue
			}
			okCount++
			if created == nil { // surface the first creative for the UI refresh hook
				created = map[string]string{
					"id": res.CreativeID, "template_id": res.TemplateID,
					"filename": res.Filename, "subject": res.Subject,
				}
			}
			lines = append(lines, fmt.Sprintf("%s: SAVED %s subject=%q money_urls=%d registry_id=%s",
				site, res.Filename, res.Subject, res.MoneyURLs, res.CreativeID))
		}
		action = fmt.Sprintf("Generated %d/%d creatives (%s)", okCount, len(sites), req.PrimaryBrandKey)
		return strings.Join(lines, "\n"), action, created

	case "list_creatives":
		rows, err := ca.db.QueryContext(r.Context(), `
			SELECT id, offer_key, brand_code, filename, subject, source, generated_at::date
			FROM mailing_creatives
			WHERE organization_id = $1
			  AND ($2 = '' OR offer_key ILIKE '%' || $2 || '%')
			  AND ($3 = '' OR brand_code ILIKE $3)
			ORDER BY generated_at DESC LIMIT 30`,
			orgID, getS("offer"), getS("brand"))
		if err != nil {
			return "ERROR: " + err.Error(), "", nil
		}
		defer rows.Close()
		var lines []string
		for rows.Next() {
			var id, offer, brand, fname, subject, source, day string
			if rows.Scan(&id, &offer, &brand, &fname, &subject, &source, &day) == nil {
				lines = append(lines, fmt.Sprintf("%s | %s %s | %s | %q | %s", id, offer, brand, day, subject, source))
			}
		}
		if len(lines) == 0 {
			return "registry is empty for that filter", "", nil
		}
		return strings.Join(lines, "\n"), "", nil

	case "get_creative":
		var subject, preheader, html string
		err := ca.db.QueryRowContext(r.Context(),
			`SELECT subject, preheader, html_content FROM mailing_creatives WHERE id = $1 AND organization_id = $2`,
			getS("id"), orgID).Scan(&subject, &preheader, &html)
		if err != nil {
			return "ERROR: " + err.Error(), "", nil
		}
		excerpt := html
		if len(excerpt) > 1500 {
			excerpt = excerpt[:1500] + "…"
		}
		return fmt.Sprintf("subject=%q preheader=%q html_bytes=%d\n--- html excerpt ---\n%s", subject, preheader, len(html), excerpt), "", nil

	case "update_creative_copy":
		id := getS("id")
		subject, preheader := getS("subject"), getS("preheader")
		if subject == "" && preheader == "" {
			return "ERROR: provide subject and/or preheader", "", nil
		}
		var templateID sql.NullString
		err := ca.db.QueryRowContext(r.Context(), `
			UPDATE mailing_creatives SET
				subject = COALESCE(NULLIF($3,''), subject),
				preheader = COALESCE(NULLIF($4,''), preheader),
				updated_at = NOW()
			WHERE id = $1 AND organization_id = $2
			RETURNING template_id`, id, orgID, subject, preheader).Scan(&templateID)
		if err != nil {
			return "ERROR: " + err.Error(), "", nil
		}
		if templateID.Valid && templateID.String != "" {
			ca.db.ExecContext(r.Context(), `
				UPDATE mailing_templates SET
					subject = COALESCE(NULLIF($2,''), subject),
					preview_text = COALESCE(NULLIF($3,''), preview_text),
					updated_at = NOW()
				WHERE id = $1`, templateID.String, subject, preheader)
		}
		return "updated", "Updated copy on " + id, nil

	default:
		return "ERROR: unknown tool " + name, "", nil
	}
}

// ---------------------------------------------------------------------------
// Conversation persistence
// ---------------------------------------------------------------------------

func (ca *CreativeAgent) persist(ctx context.Context, convoID, role, content string, toolCalls []byte, toolCallID string) {
	var tc interface{}
	if len(toolCalls) > 0 {
		tc = string(toolCalls)
	}
	if _, err := ca.db.ExecContext(ctx,
		`INSERT INTO creative_agent_messages (conversation_id, role, content, tool_calls, tool_call_id) VALUES ($1,$2,$3,$4,$5)`,
		convoID, role, content, tc, toolCallID); err != nil {
		log.Printf("[CreativeAgent] persist: %v", err)
	}
}

func (ca *CreativeAgent) loadHistory(ctx context.Context, convoID string, limit int) []agentOpenAIMsg {
	rows, err := ca.db.QueryContext(ctx, `
		SELECT role, COALESCE(content,''), COALESCE(tool_calls::text,''), COALESCE(tool_call_id,'')
		FROM creative_agent_messages WHERE conversation_id = $1
		ORDER BY created_at DESC LIMIT $2`, convoID, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var history []agentOpenAIMsg
	for rows.Next() {
		var role, content, tcJSON, tcID string
		if rows.Scan(&role, &content, &tcJSON, &tcID) != nil {
			continue
		}
		if len(content) > 2800 {
			content = content[:2800] + "\n...[truncated]"
		}
		msg := agentOpenAIMsg{Role: role, Content: content, ToolCallID: tcID}
		if tcJSON != "" && tcJSON != "null" {
			_ = json.Unmarshal([]byte(tcJSON), &msg.ToolCalls)
		}
		history = append(history, msg)
	}
	for i, j := 0, len(history)-1; i < j; i, j = i+1, j-1 {
		history[i], history[j] = history[j], history[i]
	}
	return history
}

func (ca *CreativeAgent) HandleListConversations(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	rows, err := ca.db.QueryContext(r.Context(), `
		SELECT id, title, message_count, updated_at FROM creative_agent_conversations
		WHERE organization_id = $1 ORDER BY updated_at DESC LIMIT 50`, orgID)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()
	type convo struct {
		ID           string `json:"id"`
		Title        string `json:"title"`
		MessageCount int    `json:"message_count"`
		UpdatedAt    string `json:"updated_at"`
	}
	out := []convo{}
	for rows.Next() {
		var c convo
		if rows.Scan(&c.ID, &c.Title, &c.MessageCount, &c.UpdatedAt) == nil {
			out = append(out, c)
		}
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"conversations": out})
}

func (ca *CreativeAgent) HandleGetConversation(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	id := chi.URLParam(r, "id")
	var exists bool
	if err := ca.db.QueryRowContext(r.Context(),
		`SELECT TRUE FROM creative_agent_conversations WHERE id = $1 AND organization_id = $2`,
		id, orgID).Scan(&exists); err != nil {
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "conversation not found"})
		return
	}
	rows, err := ca.db.QueryContext(r.Context(), `
		SELECT role, COALESCE(content,''), created_at FROM creative_agent_messages
		WHERE conversation_id = $1 AND role IN ('user','assistant') AND COALESCE(content,'') != ''
		ORDER BY created_at ASC LIMIT 200`, id)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()
	type msg struct {
		Role      string `json:"role"`
		Content   string `json:"content"`
		CreatedAt string `json:"created_at"`
	}
	out := []msg{}
	for rows.Next() {
		var m msg
		if rows.Scan(&m.Role, &m.Content, &m.CreatedAt) == nil {
			out = append(out, m)
		}
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"messages": out})
}
