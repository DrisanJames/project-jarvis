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
	"strings"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/config"
)

// CampaignCopilot provides an AI chat interface for campaign management.
type CampaignCopilot struct {
	db         *sql.DB
	openAIKey  string
	model      string
	httpClient *http.Client
	pmtaSvc    *PMTACampaignService
	segAPI     *SegmentationAPI
}

func NewCampaignCopilot(db *sql.DB, cfg config.OpenAIConfig, pmtaSvc *PMTACampaignService, segAPI *SegmentationAPI) *CampaignCopilot {
	model := cfg.Model
	if model == "" {
		model = "gpt-4.1"
	}
	return &CampaignCopilot{
		db:        db,
		openAIKey: cfg.APIKey,
		model:     model,
		httpClient: &http.Client{
			Timeout: 180 * time.Second,
		},
		pmtaSvc: pmtaSvc,
		segAPI:  segAPI,
	}
}

type copilotChatRequest struct {
	Message string              `json:"message"`
	History []copilotChatMsg    `json:"history"`
}

type copilotChatMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type copilotChatResponse struct {
	Response     string   `json:"response"`
	Suggestions  []string `json:"suggestions"`
	ActionsTaken []string `json:"actions_taken"`
	AIPowered    bool     `json:"ai_powered"`
}

type copilotOpenAIMsg struct {
	Role       string          `json:"role"`
	Content    string          `json:"content,omitempty"`
	ToolCalls  []copilotToolCall `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

type copilotToolCall struct {
	ID       string                `json:"id"`
	Type     string                `json:"type"`
	Function copilotFunctionCall   `json:"function"`
}

type copilotFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type copilotToolDef struct {
	Type     string              `json:"type"`
	Function copilotToolFuncDef  `json:"function"`
}

type copilotToolFuncDef struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Parameters  interface{} `json:"parameters"`
}

type copilotOpenAIReq struct {
	Model             string             `json:"model"`
	Messages          []copilotOpenAIMsg `json:"messages"`
	Tools             []copilotToolDef   `json:"tools,omitempty"`
	Temperature       float64            `json:"temperature"`
	MaxCompletionTokens int              `json:"max_completion_tokens,omitempty"`
}

type copilotOpenAIResp struct {
	Choices []struct {
		Message      copilotOpenAIMsg `json:"message"`
		FinishReason string           `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (c *CampaignCopilot) HandleChat(w http.ResponseWriter, r *http.Request) {
	if c.openAIKey == "" {
		respondJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "AI not configured"})
		return
	}

	var req copilotChatRequest
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

	messages := []copilotOpenAIMsg{
		{Role: "system", Content: buildCopilotSystemPrompt()},
	}
	for _, h := range req.History {
		if h.Role == "user" || h.Role == "assistant" {
			messages = append(messages, copilotOpenAIMsg{Role: h.Role, Content: h.Content})
		}
	}
	messages = append(messages, copilotOpenAIMsg{Role: "user", Content: req.Message})

	openaiReq := copilotOpenAIReq{
		Model:               c.model,
		Messages:            messages,
		Tools:               getCopilotTools(),
		Temperature:         0.3,
		MaxCompletionTokens: 8000,
	}

	var actionsTaken []string

	for i := 0; i < 15; i++ {
		resp, err := c.callOpenAI(ctx, openaiReq)
		if err != nil {
			log.Printf("[CampaignCopilot] OpenAI error: %v", err)
			respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "AI service error"})
			return
		}
		if len(resp.Choices) == 0 {
			respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "empty AI response"})
			return
		}

		choice := resp.Choices[0]

		if choice.FinishReason == "tool_calls" && len(choice.Message.ToolCalls) > 0 {
			openaiReq.Messages = append(openaiReq.Messages, choice.Message)
			for _, tc := range choice.Message.ToolCalls {
				result, action := c.executeCopilotTool(ctx, orgID, tc.Function.Name, tc.Function.Arguments)
				if action != "" {
					actionsTaken = append(actionsTaken, action)
				}
				openaiReq.Messages = append(openaiReq.Messages, copilotOpenAIMsg{
					Role:       "tool",
					Content:    result,
					ToolCallID: tc.ID,
				})
			}
			continue
		}

		content := choice.Message.Content
		suggestions := c.generateCopilotSuggestions(req.Message, content)
		respondJSON(w, http.StatusOK, copilotChatResponse{
			Response:     content,
			Suggestions:  suggestions,
			ActionsTaken: actionsTaken,
			AIPowered:    true,
		})
		return
	}

	respondJSON(w, http.StatusOK, copilotChatResponse{
		Response:    "I ran into a processing limit. Could you try rephrasing your request?",
		Suggestions: []string{"Show me scheduled campaigns", "What templates do we have?"},
		AIPowered:   true,
	})
}

func (c *CampaignCopilot) callOpenAI(ctx context.Context, req copilotOpenAIReq) (*copilotOpenAIResp, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", "https://api.openai.com/v1/chat/completions", bytes.NewBuffer(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.openAIKey)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var result copilotOpenAIResp
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parse error: %w (body: %s)", err, string(respBody[:min(len(respBody), 500)]))
	}
	if result.Error != nil {
		return nil, fmt.Errorf("OpenAI: %s", result.Error.Message)
	}
	return &result, nil
}

func (c *CampaignCopilot) generateCopilotSuggestions(userMsg, response string) []string {
	lower := strings.ToLower(userMsg + " " + response)
	var s []string
	if strings.Contains(lower, "campaign") {
		s = append(s, "Show me scheduled campaigns", "Clone a campaign")
	}
	if strings.Contains(lower, "template") {
		s = append(s, "List Quiz Fiesta templates", "List Discount Blog templates")
	}
	if strings.Contains(lower, "segment") {
		s = append(s, "List all segments", "Create a new segment")
	}
	if strings.Contains(lower, "performance") || strings.Contains(lower, "analytics") {
		s = append(s, "Show Gmail performance", "Show ISP sending insights")
	}
	if strings.Contains(lower, "list") {
		s = append(s, "Show all mailing lists", "Which lists have the most subscribers?")
	}
	if len(s) == 0 {
		s = []string{
			"Show me scheduled campaigns",
			"What templates do we have?",
			"Show ISP performance",
			"List all mailing lists",
		}
	}
	if len(s) > 4 {
		s = s[:4]
	}
	return s
}
