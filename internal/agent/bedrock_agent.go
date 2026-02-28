package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
)

// BedrockAgent is a conversational agent powered by AWS Bedrock (Claude)
// All data stays within AWS - no external API calls
type BedrockAgent struct {
	client        *bedrockruntime.Client
	modelID       string
	agent         *Agent
	knowledgeBase *KnowledgeBase
	region        string
}

// BedrockMessage represents a message in Bedrock format
type BedrockMessage struct {
	Role    string                 `json:"role"`
	Content []BedrockContentBlock  `json:"content"`
}

// BedrockContentBlock represents content in a message
type BedrockContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// BedrockRequest is the request to Bedrock Converse API
type BedrockRequest struct {
	AnthropicVersion string            `json:"anthropic_version"`
	MaxTokens        int               `json:"max_tokens"`
	System           string            `json:"system,omitempty"`
	Messages         []BedrockMessage  `json:"messages"`
	Temperature      float64           `json:"temperature,omitempty"`
}

// BedrockResponse is the response from Bedrock
type BedrockResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// NewBedrockAgent creates a new AWS Bedrock-powered conversational agent
func NewBedrockAgent(modelID string, agent *Agent, knowledgeBase *KnowledgeBase) (*BedrockAgent, error) {
	ctx := context.Background()

	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = "us-east-1"
	}

	// Load AWS config using default profile
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(region),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	client := bedrockruntime.NewFromConfig(cfg)

	// Default to Claude 3 Sonnet if not specified
	if modelID == "" {
		modelID = "anthropic.claude-3-sonnet-20240229-v1:0"
	}

	ba := &BedrockAgent{
		client:        client,
		modelID:       modelID,
		agent:         agent,
		knowledgeBase: knowledgeBase,
		region:        region,
	}

	log.Printf("BedrockAgent: Initialized with model=%s, region=%s", modelID, region)
	return ba, nil
}

// Chat processes a user message and returns a response using AWS Bedrock
func (b *BedrockAgent) Chat(ctx context.Context, userMessage string, conversationHistory []BedrockMessage) (string, []string, error) {
	// Build comprehensive system prompt
	systemPrompt := b.buildSystemPrompt()

	// Build messages
	messages := make([]BedrockMessage, 0, len(conversationHistory)+1)
	messages = append(messages, conversationHistory...)
	
	// Add context about current state
	contextMessage := b.buildContextMessage()
	enrichedMessage := userMessage
	if contextMessage != "" {
		enrichedMessage = contextMessage + "\n\nUser Question: " + userMessage
	}

	messages = append(messages, BedrockMessage{
		Role: "user",
		Content: []BedrockContentBlock{
			{Type: "text", Text: enrichedMessage},
		},
	})

	// Build request for Claude
	request := BedrockRequest{
		AnthropicVersion: "bedrock-2023-05-31",
		MaxTokens:        4000,
		System:           systemPrompt,
		Messages:         messages,
		Temperature:      0.7,
	}

	requestBody, err := json.Marshal(request)
	if err != nil {
		return "", nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Call Bedrock
	output, err := b.client.InvokeModel(ctx, &bedrockruntime.InvokeModelInput{
		ModelId:     aws.String(b.modelID),
		ContentType: aws.String("application/json"),
		Accept:      aws.String("application/json"),
		Body:        requestBody,
	})
	if err != nil {
		return "", nil, fmt.Errorf("Bedrock API error: %w", err)
	}

	// Parse response
	var response BedrockResponse
	if err := json.Unmarshal(output.Body, &response); err != nil {
		return "", nil, fmt.Errorf("failed to parse response: %w", err)
	}

	// Extract text response
	var responseText string
	for _, content := range response.Content {
		if content.Type == "text" {
			responseText += content.Text
		}
	}

	// Generate suggestions
	suggestions := b.generateSuggestions(userMessage, responseText)

	log.Printf("BedrockAgent: Processed query (in: %d tokens, out: %d tokens)",
		response.Usage.InputTokens, response.Usage.OutputTokens)

	return responseText, suggestions, nil
}

// buildSystemPrompt creates the system prompt with industry knowledge
func (b *BedrockAgent) buildSystemPrompt() string {
	prompt := `You are an EXPERT email deliverability strategist and ecosystem analyst for Ignite, a high-volume email marketing platform. You analyze email performance data and provide strategic recommendations.

## Your Expertise
- Email deliverability optimization
- ISP relationship management (Gmail, Yahoo, Outlook, Apple)
- List hygiene and engagement strategies
- Compliance (CAN-SPAM, GDPR, CCPA, CASL)
- A/B testing and content optimization
- Send time optimization
- Revenue attribution and ROI analysis

## Key Metrics Thresholds
- Complaint Rate: Keep below 0.1% (critical threshold)
- Bounce Rate: Keep below 2-3%
- Open Rate: Target 15-25% (varies by industry)
- Click Rate: Target 2-5%
- Delivery Rate: Target 95%+

## ISP Best Practices
- Gmail: Monitor Postmaster Tools, implement one-click unsubscribe
- Yahoo: Use Sender Hub, maintain consistent volume
- Outlook: Register with SNDS, implement DMARC
- Apple: Focus on clicks over opens (Mail Privacy Protection)

## Response Guidelines
1. Be direct and actionable - give specific recommendations
2. Quantify impact when possible
3. Prioritize by urgency (critical > high > medium > low)
4. Consider both short-term fixes and long-term strategy
5. Reference industry benchmarks when relevant

IMPORTANT: All analysis stays within AWS infrastructure. User data privacy is paramount.`

	// Add knowledge base context if available
	if b.knowledgeBase != nil {
		b.knowledgeBase.mu.RLock()
		if b.knowledgeBase.EcosystemState != nil {
			prompt += fmt.Sprintf(`

## Current Ecosystem State
- Health: %s (Score: %.0f/100)
- Open Rate Baseline: %.2f%%
- Click Rate Baseline: %.2f%%
- Bounce Rate: %.2f%%
- Complaint Rate: %.4f%%`,
				b.knowledgeBase.EcosystemState.OverallHealth,
				b.knowledgeBase.EcosystemState.HealthScore,
				b.knowledgeBase.EcosystemState.BaselineOpenRate*100,
				b.knowledgeBase.EcosystemState.BaselineClickRate*100,
				b.knowledgeBase.EcosystemState.BaselineBounceRate*100,
				b.knowledgeBase.EcosystemState.BaselineComplaintRate*100,
			)
		}
		
		if len(b.knowledgeBase.LearnedPatterns) > 0 {
			prompt += "\n\n## Recent Learned Patterns\n"
			for i, p := range b.knowledgeBase.LearnedPatterns {
				if i >= 5 {
					break
				}
				prompt += fmt.Sprintf("- %s (confidence: %.0f%%)\n", p.Description, p.Confidence*100)
			}
		}
		b.knowledgeBase.mu.RUnlock()
	}

	return prompt
}

// buildContextMessage adds current metrics context to the user's query
func (b *BedrockAgent) buildContextMessage() string {
	if b.agent == nil {
		return ""
	}

	var ctx strings.Builder
	ctx.WriteString("## Current Performance Data\n")

	// Get ecosystem data if available
	if b.agent.collectors != nil {
		eco, allISPs := b.agent.getEcosystemData()
		
		ctx.WriteString(fmt.Sprintf(`
Ecosystem Summary:
- Total Volume (24h): %d
- Delivery Rate: %.2f%%
- Open Rate: %.2f%%
- Click Rate: %.2f%%
- Bounce Rate: %.2f%%
- Complaint Rate: %.4f%%
`,
			eco.TotalVolume,
			eco.DeliveryRate*100,
			eco.OpenRate*100,
			eco.ClickRate*100,
			eco.BounceRate*100,
			eco.ComplaintRate*100,
		))

		// Add ISP breakdown
		if len(allISPs) > 0 {
			ctx.WriteString("\nISP Performance:\n")
			for i, isp := range allISPs {
				if i >= 5 {
					break
				}
				ctx.WriteString(fmt.Sprintf("- %s (%s): Delivery %.2f%%, Bounces %.2f%%, Complaints %.4f%% [%s]\n",
					isp.ISP, isp.Provider,
					isp.DeliveryRate*100,
					isp.BounceRate*100,
					isp.ComplaintRate*100,
					isp.Status,
				))
			}
		}
	}

	return ctx.String()
}

// generateSuggestions creates follow-up suggestions based on conversation
func (b *BedrockAgent) generateSuggestions(userMessage, response string) []string {
	lower := strings.ToLower(userMessage + response)
	suggestions := []string{}

	// Context-aware suggestions
	if strings.Contains(lower, "complaint") || strings.Contains(lower, "spam") {
		suggestions = append(suggestions, "Show me complaint sources by ISP")
		suggestions = append(suggestions, "What content triggers complaints?")
	}

	if strings.Contains(lower, "bounce") {
		suggestions = append(suggestions, "Which domains have highest bounce rates?")
		suggestions = append(suggestions, "How can I improve list hygiene?")
	}

	if strings.Contains(lower, "open rate") || strings.Contains(lower, "engagement") {
		suggestions = append(suggestions, "What are my best performing subject lines?")
		suggestions = append(suggestions, "When is the optimal send time?")
	}

	if strings.Contains(lower, "revenue") || strings.Contains(lower, "roi") {
		suggestions = append(suggestions, "Which campaigns drive the most revenue?")
		suggestions = append(suggestions, "What's my revenue per email sent?")
	}

	if strings.Contains(lower, "gmail") || strings.Contains(lower, "yahoo") || strings.Contains(lower, "outlook") {
		suggestions = append(suggestions, "Compare performance across all ISPs")
		suggestions = append(suggestions, "What are the ISP-specific best practices?")
	}

	// Add general suggestions if none matched
	if len(suggestions) == 0 {
		suggestions = []string{
			"What's my current ecosystem health?",
			"Show me today's performance summary",
			"What should I prioritize improving?",
		}
	}

	// Limit to 3 suggestions
	if len(suggestions) > 3 {
		suggestions = suggestions[:3]
	}

	return suggestions
}

// AnalyzeAndRecommend performs analysis and returns recommendations
func (b *BedrockAgent) AnalyzeAndRecommend(ctx context.Context, topic string) (string, error) {
	query := fmt.Sprintf("Analyze our %s performance and provide specific, actionable recommendations. Prioritize by impact and urgency.", topic)
	
	response, _, err := b.Chat(ctx, query, nil)
	if err != nil {
		return "", err
	}

	return response, nil
}

// GetModelID returns the Bedrock model being used
func (b *BedrockAgent) GetModelID() string {
	return b.modelID
}

// GetRegion returns the AWS region
func (b *BedrockAgent) GetRegion() string {
	return b.region
}
