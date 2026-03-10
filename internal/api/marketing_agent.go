package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ignite/sparkpost-monitor/internal/config"
	"github.com/ignite/sparkpost-monitor/internal/mailing"
)

// EmailMarketingAgent is a standalone AI email marketing strategist agent.
type EmailMarketingAgent struct {
	db         *sql.DB
	openAIKey  string
	model      string
	httpClient *http.Client
	pmtaSvc    *PMTACampaignService
	segAPI     *SegmentationAPI
	aiContent  *mailing.AIContentService
}

func NewEmailMarketingAgent(db *sql.DB, cfg config.OpenAIConfig, pmtaSvc *PMTACampaignService, segAPI *SegmentationAPI) *EmailMarketingAgent {
	model := cfg.Model
	if model == "" {
		model = "gpt-4.1"
	}
	return &EmailMarketingAgent{
		db:        db,
		openAIKey: cfg.APIKey,
		model:     model,
		httpClient: &http.Client{
			Timeout: 180 * time.Second,
		},
		pmtaSvc:   pmtaSvc,
		segAPI:    segAPI,
		aiContent: mailing.NewAIContentService(db, os.Getenv("ANTHROPIC_API_KEY"), cfg.APIKey),
	}
}

// ---------------------------------------------------------------------------
// Request / response types (agent-prefixed to avoid collisions with copilot)
// ---------------------------------------------------------------------------

type agentChatRequest struct {
	Message        string `json:"message"`
	ConversationID string `json:"conversation_id,omitempty"`
}

type agentChatResponse struct {
	Response               string   `json:"response"`
	ConversationID         string   `json:"conversation_id"`
	ConversationTitle      string   `json:"conversation_title"`
	ActionsTaken           []string `json:"actions_taken"`
	RecommendationsCreated []string `json:"recommendations_created,omitempty"`
}

type agentOpenAIMsg struct {
	Role       string          `json:"role"`
	Content    string          `json:"content,omitempty"`
	ToolCalls  []agentToolCall `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

type agentToolCall struct {
	ID       string            `json:"id"`
	Type     string            `json:"type"`
	Function agentFunctionCall `json:"function"`
}

type agentFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type agentToolDef struct {
	Type     string           `json:"type"`
	Function agentToolFuncDef `json:"function"`
}

type agentToolFuncDef struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Parameters  interface{} `json:"parameters"`
}

type agentOpenAIReq struct {
	Model               string           `json:"model"`
	Messages            []agentOpenAIMsg `json:"messages"`
	Tools               []agentToolDef   `json:"tools,omitempty"`
	Temperature         float64          `json:"temperature"`
	MaxCompletionTokens int              `json:"max_completion_tokens,omitempty"`
}

type agentOpenAIResp struct {
	Choices []struct {
		Message      agentOpenAIMsg `json:"message"`
		FinishReason string         `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// ---------------------------------------------------------------------------
// HandleChat — main conversational endpoint
// ---------------------------------------------------------------------------

func (a *EmailMarketingAgent) HandleChat(w http.ResponseWriter, r *http.Request) {
	if a.openAIKey == "" {
		respondJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "AI not configured"})
		return
	}

	var req agentChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "message is required"})
		return
	}

	orgID := getOrgID(r)
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	// ── Load or create conversation ──────────────────────────────────────
	convoID := req.ConversationID
	convoTitle := ""

	if convoID != "" {
		var ownerOrg string
		err := a.db.QueryRowContext(ctx,
			`SELECT organization_id::text, COALESCE(title,'') FROM agent_conversations WHERE id = $1`,
			convoID,
		).Scan(&ownerOrg, &convoTitle)
		if err == sql.ErrNoRows {
			respondJSON(w, http.StatusNotFound, map[string]string{"error": "conversation not found"})
			return
		} else if err != nil {
			log.Printf("[MarketingAgent] load conversation: %v", err)
			respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "database error"})
			return
		}
		if ownerOrg != orgID {
			respondJSON(w, http.StatusForbidden, map[string]string{"error": "access denied"})
			return
		}
	} else {
		err := a.db.QueryRowContext(ctx,
			`INSERT INTO agent_conversations (organization_id) VALUES ($1) RETURNING id::text`,
			orgID,
		).Scan(&convoID)
		if err != nil {
			log.Printf("[MarketingAgent] create conversation: %v", err)
			respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "database error"})
			return
		}
	}

	// ── Load recent message history ──────────────────────────────────────
	rows, err := a.db.QueryContext(ctx,
		`SELECT role, COALESCE(content,''), COALESCE(tool_calls::text,''), COALESCE(tool_call_id,'')
		   FROM agent_messages
		  WHERE conversation_id = $1
		  ORDER BY created_at DESC LIMIT 40`, convoID)
	if err != nil {
		log.Printf("[MarketingAgent] load messages: %v", err)
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "database error"})
		return
	}
	defer rows.Close()

	var history []agentOpenAIMsg
	for rows.Next() {
		var role, content, toolCallsJSON, toolCallID string
		if err := rows.Scan(&role, &content, &toolCallsJSON, &toolCallID); err != nil {
			continue
		}
		// Truncate large tool-result messages in history to prevent token explosion.
		// The agent saw the full result on the original turn; subsequent turns only need a summary.
		if role == "tool" && len(content) > 3000 {
			content = content[:2800] + "\n...[truncated — " + fmt.Sprintf("%d", len(content)) + " chars total]"
		}
		msg := agentOpenAIMsg{Role: role, Content: content, ToolCallID: toolCallID}
		if toolCallsJSON != "" && toolCallsJSON != "null" {
			_ = json.Unmarshal([]byte(toolCallsJSON), &msg.ToolCalls)
		}
		history = append(history, msg)
	}
	// Reverse so oldest first
	for i, j := 0, len(history)-1; i < j; i, j = i+1, j-1 {
		history[i], history[j] = history[j], history[i]
	}

	// ── Load memories & strategies ───────────────────────────────────────
	memories := a.loadMemories(ctx, orgID)
	strategies := a.loadDomainStrategies(ctx, orgID)

	// ── Build OpenAI messages ────────────────────────────────────────────
	messages := []agentOpenAIMsg{
		{Role: "system", Content: buildAgentSystemPrompt(memories, strategies)},
	}
	messages = append(messages, history...)
	messages = append(messages, agentOpenAIMsg{Role: "user", Content: req.Message})

	// Persist user message
	a.persistMessage(ctx, convoID, "user", req.Message, nil, "")

	openaiReq := agentOpenAIReq{
		Model:               a.model,
		Messages:            messages,
		Tools:               getAgentTools(),
		Temperature:         0.3,
		MaxCompletionTokens: 8000,
	}

	// ── Tool-calling loop ────────────────────────────────────────────────
	var actionsTaken []string
	var recommendationsCreated []string
	var assistantContent string

	for i := 0; i < 15; i++ {
		resp, err := a.callAgentOpenAI(ctx, openaiReq)
		if err != nil {
			log.Printf("[MarketingAgent] OpenAI error: %v", err)
			respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "AI service error"})
			return
		}
		if len(resp.Choices) == 0 {
			respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "empty AI response"})
			return
		}

		choice := resp.Choices[0]

		if choice.FinishReason == "tool_calls" && len(choice.Message.ToolCalls) > 0 {
			// Persist the assistant tool-call message
			tcJSON, _ := json.Marshal(choice.Message.ToolCalls)
			a.persistMessage(ctx, convoID, "assistant", "", tcJSON, "")

			openaiReq.Messages = append(openaiReq.Messages, choice.Message)
			for _, tc := range choice.Message.ToolCalls {
				result, action := a.executeAgentTool(ctx, orgID, tc.Function.Name, tc.Function.Arguments)
				if action != "" {
					actionsTaken = append(actionsTaken, action)
				}
				if strings.Contains(strings.ToLower(action), "recommendation") {
					recommendationsCreated = append(recommendationsCreated, action)
				}

				a.persistMessage(ctx, convoID, "tool", result, nil, tc.ID)

				openaiReq.Messages = append(openaiReq.Messages, agentOpenAIMsg{
					Role:       "tool",
					Content:    result,
					ToolCallID: tc.ID,
				})
			}
			continue
		}

		assistantContent = choice.Message.Content
		break
	}

	if assistantContent == "" {
		assistantContent = "I ran into a processing limit. Could you try rephrasing your request?"
	}

	// Persist assistant response
	a.persistMessage(ctx, convoID, "assistant", assistantContent, nil, "")

	// ── Update conversation metadata ─────────────────────────────────────
	if convoTitle == "" {
		convoTitle = strings.TrimSpace(req.Message)
		if len(convoTitle) > 60 {
			convoTitle = convoTitle[:60]
		}
		_, _ = a.db.ExecContext(ctx,
			`UPDATE agent_conversations SET title = $1, message_count = message_count + 2, updated_at = NOW() WHERE id = $2`,
			convoTitle, convoID)
	} else {
		_, _ = a.db.ExecContext(ctx,
			`UPDATE agent_conversations SET message_count = message_count + 2, updated_at = NOW() WHERE id = $1`,
			convoID)
	}

	// Async memory extraction
	go a.extractMemories(context.Background(), orgID, convoID, req.Message, assistantContent)

	if actionsTaken == nil {
		actionsTaken = []string{}
	}
	if recommendationsCreated == nil {
		recommendationsCreated = []string{}
	}

	respondJSON(w, http.StatusOK, agentChatResponse{
		Response:               assistantContent,
		ConversationID:         convoID,
		ConversationTitle:      convoTitle,
		ActionsTaken:           actionsTaken,
		RecommendationsCreated: recommendationsCreated,
	})
}

// ---------------------------------------------------------------------------
// HandleListConversations — list recent conversations for the org
// ---------------------------------------------------------------------------

func (a *EmailMarketingAgent) HandleListConversations(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	ctx := r.Context()

	rows, err := a.db.QueryContext(ctx,
		`SELECT id::text, COALESCE(title,''), COALESCE(summary,''), message_count, created_at, updated_at
		   FROM agent_conversations
		  WHERE organization_id = $1
		  ORDER BY updated_at DESC LIMIT 50`, orgID)
	if err != nil {
		log.Printf("[MarketingAgent] list conversations: %v", err)
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "database error"})
		return
	}
	defer rows.Close()

	type convoSummary struct {
		ID           string    `json:"id"`
		Title        string    `json:"title"`
		Summary      string    `json:"summary"`
		MessageCount int       `json:"message_count"`
		CreatedAt    time.Time `json:"created_at"`
		UpdatedAt    time.Time `json:"updated_at"`
	}

	var convos []convoSummary
	for rows.Next() {
		var c convoSummary
		if err := rows.Scan(&c.ID, &c.Title, &c.Summary, &c.MessageCount, &c.CreatedAt, &c.UpdatedAt); err != nil {
			continue
		}
		convos = append(convos, c)
	}
	if convos == nil {
		convos = []convoSummary{}
	}

	respondJSON(w, http.StatusOK, convos)
}

// ---------------------------------------------------------------------------
// HandleGetConversation — retrieve a conversation with its full messages
// ---------------------------------------------------------------------------

func (a *EmailMarketingAgent) HandleGetConversation(w http.ResponseWriter, r *http.Request) {
	convoID := chi.URLParam(r, "id")
	orgID := getOrgID(r)
	ctx := r.Context()

	var title, summary string
	var msgCount int
	var createdAt, updatedAt time.Time
	err := a.db.QueryRowContext(ctx,
		`SELECT COALESCE(title,''), COALESCE(summary,''), message_count, created_at, updated_at
		   FROM agent_conversations
		  WHERE id = $1 AND organization_id = $2`, convoID, orgID,
	).Scan(&title, &summary, &msgCount, &createdAt, &updatedAt)
	if err == sql.ErrNoRows {
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "conversation not found"})
		return
	} else if err != nil {
		log.Printf("[MarketingAgent] get conversation: %v", err)
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "database error"})
		return
	}

	rows, err := a.db.QueryContext(ctx,
		`SELECT id::text, role, COALESCE(content,''), COALESCE(tool_calls::text,''), COALESCE(tool_call_id,''), created_at
		   FROM agent_messages
		  WHERE conversation_id = $1
		  ORDER BY created_at`, convoID)
	if err != nil {
		log.Printf("[MarketingAgent] get messages: %v", err)
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "database error"})
		return
	}
	defer rows.Close()

	type msgEntry struct {
		ID         string          `json:"id"`
		Role       string          `json:"role"`
		Content    string          `json:"content"`
		ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`
		ToolCallID string          `json:"tool_call_id,omitempty"`
		CreatedAt  time.Time       `json:"created_at"`
	}

	var msgs []msgEntry
	for rows.Next() {
		var m msgEntry
		var tcJSON string
		if err := rows.Scan(&m.ID, &m.Role, &m.Content, &tcJSON, &m.ToolCallID, &m.CreatedAt); err != nil {
			continue
		}
		if tcJSON != "" && tcJSON != "null" {
			m.ToolCalls = json.RawMessage(tcJSON)
		}
		msgs = append(msgs, m)
	}
	if msgs == nil {
		msgs = []msgEntry{}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"conversation": map[string]interface{}{
			"id":            convoID,
			"title":         title,
			"summary":       summary,
			"message_count": msgCount,
			"created_at":    createdAt,
			"updated_at":    updatedAt,
		},
		"messages": msgs,
	})
}

// ---------------------------------------------------------------------------
// HandleDeleteConversation — delete a conversation (cascades to messages)
// ---------------------------------------------------------------------------

func (a *EmailMarketingAgent) HandleDeleteConversation(w http.ResponseWriter, r *http.Request) {
	convoID := chi.URLParam(r, "id")
	orgID := getOrgID(r)
	ctx := r.Context()

	result, err := a.db.ExecContext(ctx,
		`DELETE FROM agent_conversations WHERE id = $1 AND organization_id = $2`,
		convoID, orgID)
	if err != nil {
		log.Printf("[MarketingAgent] delete conversation: %v", err)
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "database error"})
		return
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "conversation not found"})
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// callAgentOpenAI — POST to OpenAI chat completions
// ---------------------------------------------------------------------------

func (a *EmailMarketingAgent) callAgentOpenAI(ctx context.Context, req agentOpenAIReq) (*agentOpenAIResp, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", "https://api.openai.com/v1/chat/completions", bytes.NewBuffer(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+a.openAIKey)

	resp, err := a.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var result agentOpenAIResp
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parse error: %w (body: %s)", err, string(respBody[:min(len(respBody), 500)]))
	}
	if result.Error != nil {
		return nil, fmt.Errorf("OpenAI: %s", result.Error.Message)
	}
	return &result, nil
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

func (a *EmailMarketingAgent) persistMessage(ctx context.Context, convoID, role, content string, toolCalls json.RawMessage, toolCallID string) {
	var tcVal interface{}
	if len(toolCalls) > 0 {
		tcVal = string(toolCalls)
	}
	_, err := a.db.ExecContext(ctx,
		`INSERT INTO agent_messages (conversation_id, role, content, tool_calls, tool_call_id) VALUES ($1, $2, $3, $4, $5)`,
		convoID, role, content, tcVal, toolCallID)
	if err != nil {
		log.Printf("[MarketingAgent] persist message: %v", err)
	}
}

func (a *EmailMarketingAgent) loadMemories(ctx context.Context, orgID string) []string {
	rows, err := a.db.QueryContext(ctx,
		`SELECT category || ': ' || fact FROM agent_memory WHERE organization_id = $1 AND active = true ORDER BY created_at DESC LIMIT 50`,
		orgID)
	if err != nil {
		log.Printf("[MarketingAgent] load memories: %v", err)
		return nil
	}
	defer rows.Close()

	var memories []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err == nil {
			memories = append(memories, m)
		}
	}
	return memories
}

func (a *EmailMarketingAgent) loadDomainStrategies(ctx context.Context, orgID string) []string {
	rows, err := a.db.QueryContext(ctx,
		`SELECT sending_domain || ' (' || strategy || '): ' || params::text FROM agent_domain_strategies WHERE organization_id = $1`,
		orgID)
	if err != nil {
		log.Printf("[MarketingAgent] load strategies: %v", err)
		return nil
	}
	defer rows.Close()

	var strategies []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err == nil {
			strategies = append(strategies, s)
		}
	}
	return strategies
}
