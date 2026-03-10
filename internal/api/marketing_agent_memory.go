package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// extractMemories runs asynchronously after each conversation turn to persist notable facts.
func (a *EmailMarketingAgent) extractMemories(ctx context.Context, orgID, convoID, userMsg, assistantMsg string) {
	if a.openAIKey == "" || (strings.TrimSpace(userMsg) == "" && strings.TrimSpace(assistantMsg) == "") {
		return
	}

	systemPrompt := `You are a memory extraction engine. Given a conversation exchange between a user and an email marketing assistant, extract any notable facts the assistant should remember for future conversations.

Return a JSON array of objects with:
- "category": one of preference, decision, strategy, observation, constraint
- "fact": a concise factual statement

Examples:
- {"category": "preference", "fact": "User prefers 6am MST send times"}
- {"category": "decision", "fact": "User approved 10% daily volume increase for quizfiesta.com"}
- {"category": "strategy", "fact": "quizfiesta.com is in warmup phase targeting 100K Gmail by end of March"}

Return [] if nothing notable was said.`

	userContent := "User said: " + userMsg + "\n\nAssistant said: " + assistantMsg

	req := agentOpenAIReq{
		Model: a.model,
		Messages: []agentOpenAIMsg{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userContent},
		},
		Temperature:         0.1,
		MaxCompletionTokens: 1000,
	}

	resp, err := a.callAgentOpenAI(ctx, req)
	if err != nil {
		log.Printf("[MarketingAgent] memory extraction LLM error: %v", err)
		return
	}
	if len(resp.Choices) == 0 {
		return
	}

	content := strings.TrimSpace(resp.Choices[0].Message.Content)
	content = strings.TrimPrefix(content, "```json")
	content = strings.TrimPrefix(content, "```")
	content = strings.TrimSuffix(content, "```")
	content = strings.TrimSpace(content)

	var extracted []struct {
		Category string `json:"category"`
		Fact     string `json:"fact"`
	}
	if err := json.Unmarshal([]byte(content), &extracted); err != nil {
		log.Printf("[MarketingAgent] memory extraction parse error: %v content=%s", err, content[:min(len(content), 200)])
		return
	}

	for _, m := range extracted {
		if m.Fact == "" {
			continue
		}
		// Deduplicate: check if a very similar fact already exists (limit scan scope)
		var exists bool
		snippet := m.Fact
		if len(snippet) > 50 {
			snippet = snippet[:50]
		}
		a.db.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM agent_memory WHERE organization_id = $1 AND active = true AND category = $2 AND fact ILIKE '%' || $3 || '%' LIMIT 1)`,
			orgID, m.Category, snippet).Scan(&exists)
		if exists {
			continue
		}

		_, err := a.db.ExecContext(ctx,
			`INSERT INTO agent_memory (organization_id, category, fact, source_conversation_id, confidence) VALUES ($1, $2, $3, $4, 0.80)`,
			orgID, m.Category, m.Fact, convoID)
		if err != nil {
			log.Printf("[MarketingAgent] memory insert error: %v", err)
		}
	}
}

// HandleListMemory returns all active memories for the org.
func (a *EmailMarketingAgent) HandleListMemory(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	rows, err := a.db.QueryContext(r.Context(),
		`SELECT id::text, category, fact, confidence, active, created_at
		 FROM agent_memory WHERE organization_id = $1 ORDER BY created_at DESC LIMIT 100`, orgID)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()

	type memoryEntry struct {
		ID         string  `json:"id"`
		Category   string  `json:"category"`
		Fact       string  `json:"fact"`
		Confidence float64 `json:"confidence"`
		Active     bool    `json:"active"`
		CreatedAt  string  `json:"created_at"`
	}
	var memories []memoryEntry
	for rows.Next() {
		var m memoryEntry
		var createdAt time.Time
		rows.Scan(&m.ID, &m.Category, &m.Fact, &m.Confidence, &m.Active, &createdAt)
		m.CreatedAt = createdAt.Format(time.RFC3339)
		memories = append(memories, m)
	}
	if memories == nil {
		memories = []memoryEntry{}
	}
	respondJSON(w, http.StatusOK, memories)
}

// HandleDeleteMemory soft-deletes a memory.
func (a *EmailMarketingAgent) HandleDeleteMemory(w http.ResponseWriter, r *http.Request) {
	memID := chi.URLParam(r, "id")
	orgID := getOrgID(r)
	result, err := a.db.ExecContext(r.Context(),
		`UPDATE agent_memory SET active = false WHERE id = $1 AND organization_id = $2`, memID, orgID)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "memory not found"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
