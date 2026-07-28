package api

// Anthropic Messages API client for the Campaign Copilot.
//
// The copilot's tool registry and dispatch (getCopilotTools /
// executeCopilotTool) stay provider-agnostic; this file only adapts the
// OpenAI-style function definitions to Anthropic's tool shape and runs the
// tool_use / tool_result loop against the same dispatch. Raw HTTP against
// https://api.anthropic.com/v1/messages (x-api-key: env ANTHROPIC_API_KEY,
// anthropic-version: 2023-06-01) — no SDK dependency.
//
// Model routing (resolveCopilotModel):
//   - "claude-fable-5" / "claude-opus-5"  → this Anthropic path
//   - "" with ANTHROPIC_API_KEY set       → claude-fable-5 (default)
//   - "" without the key, or "gpt-*"      → the existing OpenAI path
//   - anything else                       → 400 listing the allowed set
//
// Fable 5 / Opus 5 API notes honored here: thinking is on by default (the
// `thinking` param is intentionally omitted — an explicit config 400s on
// Fable 5), sampling params (temperature) are rejected so none are sent,
// max_tokens caps thinking + response text together (hence the 16000
// headroom), and stop_reason "refusal" is handled before reading content.
// Server-side refusal fallbacks are enabled by default via
// fallbacks:"default" + the server-side-fallback-2026-07-01 beta header.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

const (
	anthropicDefaultBaseURL = "https://api.anthropic.com"
	anthropicVersion        = "2023-06-01"
	anthropicMaxToolRounds  = 8
	anthropicMaxTokens      = 16000
)

// copilotAnthropicModels is the exact allowed set for the Anthropic path.
var copilotAnthropicModels = map[string]bool{
	"claude-fable-5": true,
	"claude-opus-5":  true,
}

// copilotCtxKey carries the resolved model through context so shared tool
// dispatch code (queue_scheduler_command's requested_by stamp) can read it
// without changing the executeCopilotTool signature used by other callers.
type copilotCtxKey string

const copilotModelCtxKey copilotCtxKey = "copilot_model"

// resolveCopilotModel maps the request's optional "model" field to a
// provider + concrete model. errMsg non-empty → respond 400 with it.
func (c *CampaignCopilot) resolveCopilotModel(requested string) (provider, model, errMsg string) {
	m := strings.TrimSpace(requested)
	switch {
	case m == "":
		if c.anthropicKey != "" {
			return "anthropic", "claude-fable-5", ""
		}
		return "openai", c.model, ""
	case copilotAnthropicModels[m]:
		return "anthropic", m, ""
	case strings.HasPrefix(m, "gpt-"):
		return "openai", m, ""
	default:
		return "", "", fmt.Sprintf("unknown model %q — allowed: claude-fable-5, claude-opus-5, gpt-* (or omit for the default)", m)
	}
}

// ─── wire types ──────────────────────────────────────────────────────────────

type anthropicTool struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema interface{} `json:"input_schema"`
}

// anthropicMsg keeps Content as raw JSON so assistant turns (which may carry
// thinking blocks with signatures on Fable 5) are echoed back byte-identical,
// as the API requires.
type anthropicMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicReq struct {
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens"`
	System    string          `json:"system,omitempty"`
	Messages  []anthropicMsg  `json:"messages"`
	Tools     []anthropicTool `json:"tools,omitempty"`
	Fallbacks interface{}     `json:"fallbacks,omitempty"`
}

type anthropicAPIError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type anthropicAPIResp struct {
	Type       string             `json:"type"`
	Model      string             `json:"model"`
	StopReason string             `json:"stop_reason"`
	Content    []json.RawMessage  `json:"content"`
	Error      *anthropicAPIError `json:"error,omitempty"`
}

// anthropicBlockProbe is the minimal typed view of a content block; the raw
// bytes are what get echoed back in history.
type anthropicBlockProbe struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type anthropicToolResultBlock struct {
	Type      string `json:"type"`
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
}

func anthropicTextContent(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

// copilotToolsToAnthropic adapts the shared OpenAI-style registry to
// Anthropic's tool shape — same names, same JSON-schema parameters, so both
// providers hit the exact same executeCopilotTool dispatch.
func copilotToolsToAnthropic(defs []copilotToolDef) []anthropicTool {
	out := make([]anthropicTool, 0, len(defs))
	for _, d := range defs {
		out = append(out, anthropicTool{
			Name:        d.Function.Name,
			Description: d.Function.Description,
			InputSchema: d.Function.Parameters,
		})
	}
	return out
}

// ─── chat loop ───────────────────────────────────────────────────────────────

func (c *CampaignCopilot) handleChatAnthropic(ctx context.Context, w http.ResponseWriter, orgID, model string, req copilotChatRequest) {
	messages := []anthropicMsg{}
	for _, h := range req.History {
		if h.Role == "user" || h.Role == "assistant" {
			messages = append(messages, anthropicMsg{Role: h.Role, Content: anthropicTextContent(h.Content)})
		}
	}
	messages = append(messages, anthropicMsg{Role: "user", Content: anthropicTextContent(req.Message)})

	apiReq := anthropicReq{
		Model:     model,
		MaxTokens: anthropicMaxTokens,
		System:    buildCopilotSystemPrompt(),
		Messages:  messages,
		Tools:     copilotToolsToAnthropic(getCopilotTools()),
		Fallbacks: "default",
	}

	var actionsTaken []string

	for i := 0; i < anthropicMaxToolRounds; i++ {
		resp, err := c.callAnthropic(ctx, apiReq)
		if err != nil {
			log.Printf("[CampaignCopilot] Anthropic error: %v", err)
			respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "AI service error"})
			return
		}

		if resp.StopReason == "refusal" {
			respondJSON(w, http.StatusOK, copilotChatResponse{
				Response:     "The model declined this request (safety classifier). Please rephrase or contact the operator.",
				Suggestions:  []string{"Show me scheduled campaigns", "List scheduler commands"},
				ActionsTaken: actionsTaken,
				AIPowered:    true,
				Model:        model,
			})
			return
		}

		toolUses := []anthropicBlockProbe{}
		var textParts []string
		for _, raw := range resp.Content {
			var probe anthropicBlockProbe
			if err := json.Unmarshal(raw, &probe); err != nil {
				continue
			}
			switch probe.Type {
			case "tool_use":
				toolUses = append(toolUses, probe)
			case "text":
				if strings.TrimSpace(probe.Text) != "" {
					textParts = append(textParts, probe.Text)
				}
			}
		}

		if resp.StopReason == "tool_use" && len(toolUses) > 0 {
			// Echo the assistant turn back verbatim (raw blocks), then answer
			// every tool_use with a tool_result in ONE user message.
			assistantContent, _ := json.Marshal(resp.Content)
			apiReq.Messages = append(apiReq.Messages, anthropicMsg{Role: "assistant", Content: assistantContent})

			results := make([]anthropicToolResultBlock, 0, len(toolUses))
			for _, tu := range toolUses {
				result, action := c.executeCopilotTool(ctx, orgID, tu.Name, string(tu.Input))
				if action != "" {
					actionsTaken = append(actionsTaken, action)
				}
				results = append(results, anthropicToolResultBlock{
					Type:      "tool_result",
					ToolUseID: tu.ID,
					Content:   result,
				})
			}
			resultContent, _ := json.Marshal(results)
			apiReq.Messages = append(apiReq.Messages, anthropicMsg{Role: "user", Content: resultContent})
			continue
		}

		content := strings.Join(textParts, "\n\n")
		respondJSON(w, http.StatusOK, copilotChatResponse{
			Response:     content,
			Suggestions:  c.generateCopilotSuggestions(req.Message, content),
			ActionsTaken: actionsTaken,
			AIPowered:    true,
			Model:        model,
		})
		return
	}

	respondJSON(w, http.StatusOK, copilotChatResponse{
		Response:     "I ran into a processing limit (max tool rounds). Could you try rephrasing your request?",
		Suggestions:  []string{"List scheduler commands", "Show me scheduled campaigns"},
		ActionsTaken: actionsTaken,
		AIPowered:    true,
		Model:        model,
	})
}

func (c *CampaignCopilot) callAnthropic(ctx context.Context, req anthropicReq) (*anthropicAPIResp, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	baseURL := c.anthropicBaseURL
	if baseURL == "" {
		baseURL = anthropicDefaultBaseURL
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", baseURL+"/v1/messages", bytes.NewBuffer(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", c.anthropicKey)
	httpReq.Header.Set("anthropic-version", anthropicVersion)
	// Server-side refusal fallbacks (fallbacks:"default" in the body).
	httpReq.Header.Set("anthropic-beta", "server-side-fallback-2026-07-01")

	client := c.anthropicHTTPClient
	if client == nil {
		client = &http.Client{Timeout: 180 * time.Second}
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var result anthropicAPIResp
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("anthropic parse error: %w (body: %s)", err, string(respBody[:min(len(respBody), 500)]))
	}
	if result.Error != nil {
		return nil, fmt.Errorf("anthropic: %s: %s", result.Error.Type, result.Error.Message)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("anthropic: HTTP %d (body: %s)", resp.StatusCode, string(respBody[:min(len(respBody), 500)]))
	}
	return &result, nil
}
