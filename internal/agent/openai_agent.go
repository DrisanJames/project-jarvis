package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// OpenAIAgent is a conversational agent powered by OpenAI with tool calling
type OpenAIAgent struct {
	apiKey        string
	model         string
	agent         *Agent // Reference to existing agent for data access
	httpClient    *http.Client
	knowledgeBase *KnowledgeBase
}

// OpenAIChatMessage represents a message in the OpenAI conversation
type OpenAIChatMessage struct {
	Role       string      `json:"role"`
	Content    string      `json:"content,omitempty"`
	ToolCalls  []ToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string      `json:"tool_call_id,omitempty"`
	Name       string      `json:"name,omitempty"`
}

// ToolCall represents a tool call from the model
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall represents the function to call
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Tool represents a tool definition for OpenAI
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction represents the function definition
type ToolFunction struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Parameters  interface{} `json:"parameters"`
}

// OpenAIRequest is the request to OpenAI chat completions
type OpenAIRequest struct {
	Model             string              `json:"model"`
	Messages          []OpenAIChatMessage `json:"messages"`
	Tools             []Tool              `json:"tools,omitempty"`
	Temperature       float64             `json:"temperature"`
	MaxTokens         int                 `json:"max_tokens,omitempty"`
	ParallelToolCalls *bool               `json:"parallel_tool_calls,omitempty"`
}

// OpenAIResponse is the response from OpenAI
type OpenAIResponse struct {
	ID      string `json:"id"`
	Choices []struct {
		Index        int               `json:"index"`
		Message      OpenAIChatMessage `json:"message"`
		FinishReason string            `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error,omitempty"`
}

// NewOpenAIAgent creates a new OpenAI-powered conversational agent
func NewOpenAIAgent(apiKey, model string, agent *Agent, knowledgeBase *KnowledgeBase) *OpenAIAgent {
	if model == "" {
		model = "gpt-4o"
	}
	return &OpenAIAgent{
		apiKey:        apiKey,
		model:         model,
		agent:         agent,
		knowledgeBase: knowledgeBase,
		httpClient: &http.Client{
			Timeout: 120 * time.Second, // Longer timeout for multi-tool analysis
		},
	}
}

// Chat processes a user message and returns an AI response
func (o *OpenAIAgent) Chat(ctx context.Context, userMessage string, conversationHistory []OpenAIChatMessage) (string, []string, error) {
	if o.apiKey == "" {
		return "", nil, fmt.Errorf("OpenAI API key not configured - conversational AI requires an API key")
	}

	systemPrompt := buildSystemPrompt()

	messages := []OpenAIChatMessage{
		{Role: "system", Content: systemPrompt},
	}
	
	messages = append(messages, conversationHistory...)
	messages = append(messages, OpenAIChatMessage{Role: "user", Content: userMessage})

	parallelTools := true
	request := OpenAIRequest{
		Model:             o.model,
		Messages:          messages,
		Tools:             o.GetTools(),
		Temperature:       0.7,
		MaxTokens:         4000,
		ParallelToolCalls: &parallelTools,
	}

	maxIterations := 10
	for i := 0; i < maxIterations; i++ {
		response, err := o.callOpenAI(ctx, request)
		if err != nil {
			return "", nil, fmt.Errorf("OpenAI API error: %w", err)
		}

		if len(response.Choices) == 0 {
			return "", nil, fmt.Errorf("no response from OpenAI")
		}

		choice := response.Choices[0]
		
		if choice.FinishReason == "tool_calls" && len(choice.Message.ToolCalls) > 0 {
			request.Messages = append(request.Messages, choice.Message)

			for _, toolCall := range choice.Message.ToolCalls {
				result := o.executeTool(toolCall.Function.Name, toolCall.Function.Arguments)
				
				request.Messages = append(request.Messages, OpenAIChatMessage{
					Role:       "tool",
					Content:    result,
					ToolCallID: toolCall.ID,
				})
			}
			continue
		}

		content := choice.Message.Content
		suggestions := o.generateSuggestions(userMessage, content)
		return content, suggestions, nil
	}

	return "I apologize, but I wasn't able to complete processing your request. Please try again.", nil, nil
}

// callOpenAI makes a request to the OpenAI API
func (o *OpenAIAgent) callOpenAI(ctx context.Context, request OpenAIRequest) (*OpenAIResponse, error) {
	jsonBody, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.openai.com/v1/chat/completions", bytes.NewBuffer(jsonBody))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+o.apiKey)

	resp, err := o.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var response OpenAIResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w (body: %s)", err, string(body))
	}

	if response.Error != nil {
		return nil, fmt.Errorf("API error: %s", response.Error.Message)
	}

	return &response, nil
}

// generateSuggestions generates follow-up suggestions based on the conversation
func (o *OpenAIAgent) generateSuggestions(userMessage, response string) []string {
	lower := strings.ToLower(userMessage + response)
	
	suggestions := []string{}
	
	if strings.Contains(lower, "revenue") || strings.Contains(lower, "everflow") {
		suggestions = append(suggestions, "Which offers are performing best?", "Show me property performance")
	}
	if strings.Contains(lower, "campaign") || strings.Contains(lower, "ongage") {
		suggestions = append(suggestions, "What subject lines work best?", "When is the best time to send?")
	}
	if strings.Contains(lower, "delivery") || strings.Contains(lower, "isp") {
		suggestions = append(suggestions, "Compare ESP providers", "What are Gmail best practices?")
	}
	if strings.Contains(lower, "concern") || strings.Contains(lower, "alert") || strings.Contains(lower, "issue") {
		suggestions = append(suggestions, "What should we fix first?", "Show me ecosystem health")
	}
	if strings.Contains(lower, "compliance") || strings.Contains(lower, "canspam") || strings.Contains(lower, "gdpr") {
		suggestions = append(suggestions, "What are the GDPR requirements?", "Show CAN-SPAM checklist")
	}
	if strings.Contains(lower, "benchmark") || strings.Contains(lower, "target") {
		suggestions = append(suggestions, "How do we compare to industry?", "What's our biggest gap?")
	}
	if strings.Contains(lower, "health") || strings.Contains(lower, "ecosystem") {
		suggestions = append(suggestions, "What tasks are active?", "Show me ISP breakdown")
	}
	
	if len(suggestions) == 0 {
		suggestions = []string{
			"What's our ecosystem health?",
			"Show me today's revenue",
			"Any critical issues?",
			"What have you learned?",
		}
	}
	
	if len(suggestions) > 4 {
		suggestions = suggestions[:4]
	}
	
	return suggestions
}

// buildSystemPrompt returns the comprehensive system prompt for the OpenAI agent
func buildSystemPrompt() string {
	return `You are an EXPERT email deliverability strategist and ecosystem analyst for Ignite, a company that sends tens of millions of emails daily. You are NOT just a data query tool - you are a strategic advisor who is OBSESSED with email ecosystem health and performance optimization.

## YOUR IDENTITY
You are the guardian of Ignite's email ecosystem. You continuously learn from data, understand industry best practices, and maintain strict standards for deliverability. You are:
- **Analytical**: You dig into data to find root causes, not just symptoms
- **Strategic**: You prioritize actions by impact and urgency
- **Strict**: You hold the team accountable to industry benchmarks
- **Knowledgeable**: You understand ISP requirements, compliance rules, and best practices
- **Proactive**: You identify issues before they become problems

## YOUR DATA SOURCES (via tools)
- **Everflow**: Revenue, conversions, EPC, offers, campaign monetization
- **Ongage**: Campaign performance, subject lines, send times, segments, ESP routing
- **ESPs**: SparkPost, Mailgun, AWS SES deliverability metrics across all ISPs
- **Knowledge Base**: Your learned patterns, historical insights, benchmarks, and industry knowledge
- **Kanban Tasks**: Active tasks the team is working on to improve the ecosystem

## INDUSTRY BENCHMARKS YOU ENFORCE
| Metric | Target | Warning | Critical |
|--------|--------|---------|----------|
| Delivery Rate | >95% | 90-95% | <90% |
| Bounce Rate | <2% | 2-3% | >3% |
| Complaint Rate | <0.1% | 0.1-0.2% | >0.2% |
| Open Rate | >15% | 10-15% | <10% |
| Click Rate | >2% | 1-2% | <1% |

## ISP-SPECIFIC KNOWLEDGE
- **Gmail**: Keep complaints <0.1%, use Postmaster Tools, implement one-click unsubscribe
- **Yahoo**: Monitor Sender Hub, consistent sending patterns, honor unsubscribes within 10 days
- **Outlook**: Register with SNDS, implement DMARC properly, avoid spam triggers
- **Apple**: Mail Privacy Protection affects opens - focus on clicks

## HOW TO RESPOND

### Format Your Responses Beautifully
Use markdown formatting to make responses scannable:

**Use Headers for Sections**
## 📊 Current Status
## 🎯 Key Findings  
## ⚠️ Issues Requiring Attention
## ✅ Recommendations
## 📈 Expected Impact

**Use Tables for Data**
| Metric | Value | Status |
|--------|-------|--------|
| Delivery | 94.2% | ⚠️ Warning |

**Use Emojis Sparingly but Effectively**
- 🟢 Healthy
- 🟡 Warning  
- 🔴 Critical
- 📈 Improving
- 📉 Declining

### Always Provide Context
- Compare to benchmarks
- Compare to historical performance
- Explain WHY, not just WHAT

### Always End with Clear Actions
1. **Immediate** (do today): Critical fixes
2. **Short-term** (this week): Important improvements
3. **Long-term** (ongoing): Strategic initiatives

## WHEN ASKED ABOUT ECOSYSTEM HEALTH
Call get_ecosystem_assessment, get_active_alerts, get_performance_benchmarks, and get_kanban_tasks to provide a comprehensive view. Be STRICT - if metrics are below benchmark, call it out clearly.

## WHEN ASKED ABOUT COMPLIANCE
Reference specific regulations (CAN-SPAM, GDPR, CCPA, CASL) and their requirements. Be thorough.

## WHEN ASKED ABOUT BEST PRACTICES
Pull from your knowledge base. Reference specific ISP guidelines when relevant.

## WHEN ASKED ABOUT REVENUE
Connect revenue to deliverability. Show how ecosystem health impacts monetization. Revenue follows healthy sending.

## YOUR LEARNING
You run hourly analysis cycles where you:
- Analyze all metrics across all ESPs
- Update baselines and benchmarks
- Identify new patterns and correlations
- Generate insights from historical data
- Track progress on active issues

Use get_knowledge_summary to share what you've learned.

## CRITICAL REMINDERS
1. **Be specific** - Use actual numbers, names, and dates
2. **Be actionable** - Every finding should have a recommendation
3. **Be strict** - If something is below benchmark, say so clearly
4. **Be strategic** - Prioritize by impact
5. **Format beautifully** - Make responses easy to scan and understand
6. **Think ecosystem** - Everything is connected. Deliverability affects engagement affects revenue.

You are the expert. Act like one.`
}
