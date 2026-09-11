package contentdesk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// Environment. CONTENT_DESK_ENABLED defaults OFF: with it unset no model is
// ever called and the worker never claims a run.
const (
	EnvEnabled           = "CONTENT_DESK_ENABLED"
	EnvModelWrite        = "CONTENT_DESK_MODEL_WRITE"
	EnvModelJudge        = "CONTENT_DESK_MODEL_JUDGE"
	EnvModelLight        = "CONTENT_DESK_MODEL_LIGHT"
	EnvDailyUSD          = "CONTENT_DESK_DAILY_USD"
	EnvSimhashMaxHamming = "CONTENT_DESK_SIMHASH_MAX_HAMMING"

	DefaultModelWrite = "claude-sonnet-5"
	DefaultModelJudge = "claude-opus-5"
	DefaultModelLight = "claude-haiku-4-5"
	DefaultDailyUSD   = 25.0
)

// Tier selects a model.
type Tier string

const (
	TierWrite Tier = "write"
	TierJudge Tier = "judge"
	TierLight Tier = "light"
)

// ModelConfig is the resolved model per tier.
type ModelConfig struct {
	Write string `json:"write"`
	Judge string `json:"judge"`
	Light string `json:"light"`
}

// For returns the model id for a tier.
func (m ModelConfig) For(t Tier) string {
	switch t {
	case TierJudge:
		return m.Judge
	case TierLight:
		return m.Light
	}
	return m.Write
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// ModelsFromEnv resolves the three tiers.
func ModelsFromEnv() ModelConfig {
	return ModelConfig{
		Write: envOr(EnvModelWrite, DefaultModelWrite),
		Judge: envOr(EnvModelJudge, DefaultModelJudge),
		Light: envOr(EnvModelLight, DefaultModelLight),
	}
}

// Enabled is the kill switch (default "0" = off).
func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvEnabled))) {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}

// DailyBudgetUSD is the hard daily cap. Unset → 25. Unparseable or negative →
// 0, which refuses every call: a misconfigured budget fails closed.
func DailyBudgetUSD() float64 {
	raw := strings.TrimSpace(os.Getenv(EnvDailyUSD))
	if raw == "" {
		return DefaultDailyUSD
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v < 0 {
		return 0
	}
	return v
}

// SimhashMaxHammingFromEnv is the near-duplicate threshold (default 3).
func SimhashMaxHammingFromEnv() int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(EnvSimhashMaxHamming))); err == nil && v >= 0 {
		return v
	}
	return DefaultSimhashMaxHamming
}

// ModelPrice is USD per million tokens.
type ModelPrice struct {
	InPerMTok  float64 `json:"in_per_mtok"`
	OutPerMTok float64 `json:"out_per_mtok"`
}

// ModelPrices — list prices for the three configured defaults.
var ModelPrices = map[string]ModelPrice{
	"claude-sonnet-5":  {InPerMTok: 2, OutPerMTok: 10},
	"claude-opus-5":    {InPerMTok: 5, OutPerMTok: 25},
	"claude-haiku-4-5": {InPerMTok: 1, OutPerMTok: 5},
}

// WebSearchUSDPerThousand is the web search list price.
const WebSearchUSDPerThousand = 10.0

// Cache multipliers on the input price (5-minute TTL write, read).
const (
	cacheWriteMultiplier = 1.25
	cacheReadMultiplier  = 0.1
)

// PriceFor returns the price for a model id; a dated id matches its alias by
// prefix. An unknown model is priced at the MOST expensive known rate so the
// budget can only over-count, never under-count.
func PriceFor(model string) ModelPrice {
	if p, ok := ModelPrices[model]; ok {
		return p
	}
	for k, p := range ModelPrices {
		if strings.HasPrefix(model, k) {
			return p
		}
	}
	var max ModelPrice
	for _, p := range ModelPrices {
		if p.OutPerMTok > max.OutPerMTok {
			max = p
		}
	}
	return max
}

// CostUSD prices one response.
func CostUSD(model string, input, cacheWrite, cacheRead, output, webSearches int64) float64 {
	p := PriceFor(model)
	in := float64(input) + cacheWriteMultiplier*float64(cacheWrite) + cacheReadMultiplier*float64(cacheRead)
	return p.InPerMTok*in/1e6 + p.OutPerMTok*float64(output)/1e6 + WebSearchUSDPerThousand*float64(webSearches)/1000
}

// CategoryAllowlist is the ONLY set of domains research may search or fetch
// per category. A category not listed here cannot be researched.
// State insurance-department domains are not enumerated yet (the plan names
// them; a verified list is pending) — insurance research is limited to the
// national sources until they are added here.
var CategoryAllowlist = map[string][]string{
	CatFinance:   {"irs.gov", "consumerfinance.gov", "fdic.gov", "federalreserve.gov", "ecfr.gov", "law.cornell.edu", "sec.gov", "investor.gov"},
	CatTax:       {"irs.gov", "consumerfinance.gov", "fdic.gov", "federalreserve.gov", "ecfr.gov", "law.cornell.edu", "sec.gov", "investor.gov"},
	CatHealth:    {"cdc.gov", "nih.gov", "medlineplus.gov", "fda.gov", "cms.gov", "medicare.gov"},
	CatBenefits:  {"ssa.gov", "medicare.gov", "va.gov", "benefits.gov"},
	CatInsurance: {"naic.org", "iii.org"},
	CatHistory:   {"loc.gov", "archives.gov", "si.edu", "britannica.com"},
	CatDIY:       {"energy.gov", "cpsc.gov", "osha.gov", "nfpa.org"},
}

// ValidCategory reports whether a category has an allow-list.
func ValidCategory(c string) bool { _, ok := CategoryAllowlist[c]; return ok }

// Errors from the LLM surface.
var (
	ErrDisabled           = errors.New("content desk is disabled (CONTENT_DESK_ENABLED is not 1)")
	ErrBudgetExceeded     = errors.New("content desk daily budget exceeded")
	ErrBudgetUnavailable  = errors.New("content desk budget ledger unreadable — refusing to spend")
	ErrRefused            = errors.New("model refused")
	ErrTruncated          = errors.New("model output truncated at max_tokens")
	ErrNoStructuredOutput = errors.New("model returned no structured output")
	ErrNoAllowlist        = errors.New("no research allow-list for category")
	ErrPauseLimit         = errors.New("server-tool loop paused too many times")
)

// BudgetLedger is the daily spend ledger (content_usage_daily). Store
// implements it.
type BudgetLedger interface {
	SpentToday(ctx context.Context, orgID string) (float64, error)
	AddSpend(ctx context.Context, orgID string, usd float64) error
}

// GenerateRequest is one structured call. With WebCategory set the call gets
// web_search + web_fetch restricted to the category allow-list and returns
// its result through a strict tool named ToolName; otherwise the result is
// constrained by output_config.format.
type GenerateRequest struct {
	OrgID       string
	Tier        Tier
	System      string // stable per stage — cached
	Prompt      string
	Schema      map[string]any
	ToolName    string
	WebCategory string
	MaxTokens   int64
	// NoThinking disables the model's default thinking for stages that only
	// transcribe verified input (the draft). Thinking tokens count against
	// MaxTokens.
	NoThinking bool
}

// GenerateResult is the structured JSON plus what it cost.
type GenerateResult struct {
	JSON  json.RawMessage
	Usage Usage
}

// LLM is the model seam the pipeline depends on.
type LLM interface {
	Generate(ctx context.Context, req GenerateRequest) (*GenerateResult, error)
}

// AnthropicLLM calls Claude through the official SDK. Every call passes the
// kill switch and the daily budget first; spend is recorded after every
// response, and a ledger write failure fails the call.
type AnthropicLLM struct {
	client           anthropic.Client
	Models           ModelConfig
	Budget           BudgetLedger
	DailyLimit       func() float64
	IsEnabled        func() bool
	Sleep            func(ctx context.Context, d time.Duration) error
	MaxAttempts      int
	MaxContinuations int
}

// ContentDeskAPIKey returns the Content Desk's own Anthropic key when one is
// configured. The desk's spend is capped by CONTENT_DESK_DAILY_USD, so it can run
// on a separately funded account without moving the rest of the platform's
// Anthropic traffic. Empty = no override (the SDK reads ANTHROPIC_API_KEY).
func ContentDeskAPIKey() string {
	return strings.TrimSpace(os.Getenv("CONTENT_DESK_ANTHROPIC_API_KEY"))
}

// NewAnthropicLLM builds the client. The SDK's own retries are disabled so
// retry/backoff is explicit here (429/5xx only). The key comes from
// CONTENT_DESK_ANTHROPIC_API_KEY when set, else ANTHROPIC_API_KEY (SDK default),
// the same variable the rest of internal/api reads.
func NewAnthropicLLM(budget BudgetLedger, opts ...option.RequestOption) *AnthropicLLM {
	base := []option.RequestOption{option.WithMaxRetries(0), option.WithRequestTimeout(10 * time.Minute)}
	if k := ContentDeskAPIKey(); k != "" {
		base = append(base, option.WithAPIKey(k))
	}
	return &AnthropicLLM{
		client:           anthropic.NewClient(append(base, opts...)...),
		Models:           ModelsFromEnv(),
		Budget:           budget,
		DailyLimit:       DailyBudgetUSD,
		IsEnabled:        Enabled,
		Sleep:            sleepCtx,
		MaxAttempts:      4,
		MaxContinuations: 5,
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (l *AnthropicLLM) checkBudget(ctx context.Context, orgID string) error {
	if l.Budget == nil {
		return ErrBudgetUnavailable
	}
	spent, err := l.Budget.SpentToday(ctx, orgID)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBudgetUnavailable, err)
	}
	if limit := l.DailyLimit(); spent >= limit {
		return fmt.Errorf("%w: spent $%.4f of $%.2f", ErrBudgetExceeded, spent, limit)
	}
	return nil
}

func schemaRequired(s map[string]any) []string {
	switch r := s["required"].(type) {
	case []string:
		return r
	case []any:
		out := make([]string, 0, len(r))
		for _, v := range r {
			if str, ok := v.(string); ok {
				out = append(out, str)
			}
		}
		return out
	}
	return nil
}

// buildParams assembles the request.
func (l *AnthropicLLM) buildParams(req GenerateRequest, model string) (anthropic.MessageNewParams, error) {
	maxTok := req.MaxTokens
	if maxTok <= 0 {
		maxTok = 16000
	}
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(model),
		MaxTokens: maxTok,
		System: []anthropic.TextBlockParam{{
			Text:         req.System,
			CacheControl: anthropic.NewCacheControlEphemeralParam(),
		}},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(req.Prompt)),
		},
	}
	if req.NoThinking {
		params.Thinking = anthropic.ThinkingConfigParamUnion{OfDisabled: &anthropic.ThinkingConfigDisabledParam{}}
	}
	if req.WebCategory == "" {
		params.OutputConfig = anthropic.OutputConfigParam{
			Format: anthropic.JSONOutputFormatParam{Schema: req.Schema},
		}
		return params, nil
	}
	domains := CategoryAllowlist[req.WebCategory]
	if len(domains) == 0 {
		return params, fmt.Errorf("%w %q", ErrNoAllowlist, req.WebCategory)
	}
	if req.ToolName == "" {
		return params, errors.New("web research call needs a ToolName for its structured result")
	}
	submit := anthropic.ToolParam{
		Name:        req.ToolName,
		Description: anthropic.String("Call this exactly once, after your research is complete, to submit the final structured result. Do not call it before you have searched."),
		Strict:      anthropic.Bool(true),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties:  req.Schema["properties"],
			Required:    schemaRequired(req.Schema),
			ExtraFields: map[string]any{"additionalProperties": false},
		},
	}
	params.Tools = []anthropic.ToolUnionParam{
		{OfWebSearchTool20260209: &anthropic.WebSearchTool20260209Param{AllowedDomains: domains, MaxUses: anthropic.Int(8)}},
		{OfWebFetchTool20260209: &anthropic.WebFetchTool20260209Param{AllowedDomains: domains, MaxUses: anthropic.Int(8)}},
		{OfTool: &submit},
	}
	params.ToolChoice = anthropic.ToolChoiceUnionParam{OfAuto: &anthropic.ToolChoiceAutoParam{}}
	return params, nil
}

// Generate runs one structured call (with pause_turn continuations).
func (l *AnthropicLLM) Generate(ctx context.Context, req GenerateRequest) (*GenerateResult, error) {
	if l.IsEnabled == nil || !l.IsEnabled() {
		return nil, ErrDisabled
	}
	if err := l.checkBudget(ctx, req.OrgID); err != nil {
		return nil, err
	}
	model := l.Models.For(req.Tier)
	params, err := l.buildParams(req, model)
	if err != nil {
		return nil, err
	}
	total := Usage{Model: model}
	for cont := 0; ; cont++ {
		resp, err := l.callWithRetry(ctx, params)
		if err != nil {
			return &GenerateResult{Usage: total}, err
		}
		u := Usage{
			Model:        model,
			InputTokens:  resp.Usage.InputTokens + resp.Usage.CacheCreationInputTokens + resp.Usage.CacheReadInputTokens,
			OutputTokens: resp.Usage.OutputTokens,
			WebSearches:  resp.Usage.ServerToolUse.WebSearchRequests,
			USD: CostUSD(model, resp.Usage.InputTokens, resp.Usage.CacheCreationInputTokens,
				resp.Usage.CacheReadInputTokens, resp.Usage.OutputTokens, resp.Usage.ServerToolUse.WebSearchRequests),
		}
		total.Add(u)
		if err := l.Budget.AddSpend(ctx, req.OrgID, u.USD); err != nil {
			return &GenerateResult{Usage: total}, fmt.Errorf("usage ledger write failed (spend $%.4f unrecorded): %w", u.USD, err)
		}
		switch resp.StopReason {
		case anthropic.StopReasonRefusal:
			return &GenerateResult{Usage: total}, fmt.Errorf("%w: category=%s %v", ErrRefused, resp.StopDetails.Category, resp.StopDetails.Explanation)
		case anthropic.StopReasonMaxTokens:
			return &GenerateResult{Usage: total}, ErrTruncated
		case anthropic.StopReasonPauseTurn:
			if cont >= l.MaxContinuations {
				return &GenerateResult{Usage: total}, ErrPauseLimit
			}
			// Resume the server-side tool loop: re-send with the paused
			// assistant turn appended; no extra user message.
			params.Messages = append(params.Messages, resp.ToParam())
			// web_*_20260209 filter results in a code-execution container; the
			// continuation must name it or the API 400s "container_id is required".
			if resp.Container.ID != "" {
				params.Container = anthropic.MessageCreateParamsContainerUnion{OfString: anthropic.String(resp.Container.ID)}
			}
			if err := l.checkBudget(ctx, req.OrgID); err != nil {
				return &GenerateResult{Usage: total}, err
			}
			continue
		}
		out, err := extractStructured(resp, req)
		return &GenerateResult{JSON: out, Usage: total}, err
	}
}

func extractStructured(resp *anthropic.Message, req GenerateRequest) (json.RawMessage, error) {
	if req.WebCategory != "" {
		for _, block := range resp.Content {
			if tu, ok := block.AsAny().(anthropic.ToolUseBlock); ok && tu.Name == req.ToolName {
				return json.RawMessage(tu.Input), nil
			}
		}
		return nil, fmt.Errorf("%w: stop_reason=%s without a %s call", ErrNoStructuredOutput, resp.StopReason, req.ToolName)
	}
	var sb strings.Builder
	for _, block := range resp.Content {
		if tb, ok := block.AsAny().(anthropic.TextBlock); ok {
			sb.WriteString(tb.Text)
		}
	}
	out := strings.TrimSpace(sb.String())
	if out == "" || !json.Valid([]byte(out)) {
		return nil, fmt.Errorf("%w: text is not JSON", ErrNoStructuredOutput)
	}
	return json.RawMessage(out), nil
}

// retryable: 429 and 5xx only.
func retryable(err error) bool {
	var ae *anthropic.Error
	if errors.As(err, &ae) {
		return ae.StatusCode == 429 || ae.StatusCode >= 500
	}
	return false
}

func backoff(attempt int) time.Duration {
	d := time.Second << uint(attempt-1)
	if d > 16*time.Second {
		d = 16 * time.Second
	}
	return d
}

func (l *AnthropicLLM) callWithRetry(ctx context.Context, params anthropic.MessageNewParams) (*anthropic.Message, error) {
	max := l.MaxAttempts
	if max < 1 {
		max = 1
	}
	for attempt := 1; ; attempt++ {
		resp, err := l.client.Messages.New(ctx, params)
		if err == nil {
			return resp, nil
		}
		if !retryable(err) || attempt >= max {
			return nil, err
		}
		if serr := l.Sleep(ctx, backoff(attempt)); serr != nil {
			return nil, serr
		}
	}
}
