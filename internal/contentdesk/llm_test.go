package contentdesk

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/option"
)

// No network: every test points the real SDK client at an httptest server.

type memBudget struct {
	spent float64
	adds  []float64
	err   error
}

func (m *memBudget) SpentToday(context.Context, string) (float64, error) { return m.spent, m.err }
func (m *memBudget) AddSpend(_ context.Context, _ string, usd float64) error {
	m.spent += usd
	m.adds = append(m.adds, usd)
	return nil
}

const msgText = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-5",
 "content":[{"type":"text","text":"{\"ok\":true}"}],"stop_reason":"end_turn","stop_sequence":null,
 "usage":{"input_tokens":1000000,"output_tokens":100000,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,
          "server_tool_use":{"web_search_requests":0,"web_fetch_requests":0}}}`

func newTestLLM(t *testing.T, b *memBudget, h http.HandlerFunc) (*AnthropicLLM, *int32, *[]string) {
	t.Helper()
	var calls int32
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		w.Header().Set("Content-Type", "application/json")
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	l := NewAnthropicLLM(b, option.WithBaseURL(srv.URL), option.WithAPIKey("test-key"))
	l.IsEnabled = func() bool { return true }
	l.DailyLimit = func() float64 { return 25 }
	l.Sleep = func(context.Context, time.Duration) error { return nil }
	l.Models = ModelConfig{Write: "claude-sonnet-5", Judge: "claude-opus-5", Light: "claude-haiku-4-5"}
	return l, &calls, &bodies
}

func req() GenerateRequest {
	return GenerateRequest{OrgID: "org", Tier: TierWrite, System: "sys", Prompt: "p",
		Schema: sObj(map[string]any{"ok": sBool()}), MaxTokens: 100}
}

func TestLLM_KillSwitchDefaultsOff(t *testing.T) {
	t.Setenv(EnvEnabled, "")
	if Enabled() {
		t.Fatal("CONTENT_DESK_ENABLED unset must mean disabled")
	}
	b := &memBudget{}
	l, calls, _ := newTestLLM(t, b, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, msgText) })
	l.IsEnabled = Enabled
	if _, err := l.Generate(context.Background(), req()); !errors.Is(err, ErrDisabled) || *calls != 0 {
		t.Fatalf("disabled must refuse without a call: err=%v calls=%d", err, *calls)
	}
}

func TestLLM_BudgetExceededFailsClosed(t *testing.T) {
	b := &memBudget{spent: 25}
	l, calls, _ := newTestLLM(t, b, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, msgText) })
	if _, err := l.Generate(context.Background(), req()); !errors.Is(err, ErrBudgetExceeded) || *calls != 0 {
		t.Fatalf("over budget must refuse without a call: err=%v calls=%d", err, *calls)
	}
	b = &memBudget{err: errors.New("db down")}
	l, calls, _ = newTestLLM(t, b, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, msgText) })
	if _, err := l.Generate(context.Background(), req()); !errors.Is(err, ErrBudgetUnavailable) || *calls != 0 {
		t.Fatalf("an unreadable ledger must refuse: err=%v calls=%d", err, *calls)
	}
	t.Setenv(EnvDailyUSD, "abc")
	if DailyBudgetUSD() != 0 {
		t.Fatal("an unparseable budget must fail closed to 0")
	}
}

func TestLLM_RetriesOn429ThenRecordsCost(t *testing.T) {
	b := &memBudget{}
	var n int32
	l, calls, bodies := newTestLLM(t, b, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
			return
		}
		io.WriteString(w, msgText)
	})
	res, err := l.Generate(context.Background(), req())
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 2 || string(res.JSON) != `{"ok":true}` {
		t.Fatalf("calls=%d json=%s", *calls, res.JSON)
	}
	// Sonnet 5: 1M in × $2 + 0.1M out × $10 = $3.
	if len(b.adds) != 1 || math.Abs(b.adds[0]-3) > 1e-9 || math.Abs(res.Usage.USD-3) > 1e-9 {
		t.Fatalf("cost recorded %v usage %+v", b.adds, res.Usage)
	}
	body := (*bodies)[1]
	for _, want := range []string{`"output_config"`, `"json_schema"`, `"cache_control"`, `"claude-sonnet-5"`} {
		if !strings.Contains(body, want) {
			t.Errorf("request missing %s: %s", want, body)
		}
	}
}

func TestLLM_NoRetryOn400(t *testing.T) {
	l, calls, _ := newTestLLM(t, &memBudget{}, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`)
	})
	if _, err := l.Generate(context.Background(), req()); err == nil || *calls != 1 {
		t.Fatalf("400 must not retry: err=%v calls=%d", err, *calls)
	}
}

func TestLLM_RefusalIsTerminalAndStillBilled(t *testing.T) {
	b := &memBudget{}
	l, _, _ := newTestLLM(t, b, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, strings.Replace(strings.Replace(msgText, `"end_turn"`, `"refusal"`, 1),
			`"stop_sequence":null`, `"stop_sequence":null,"stop_details":{"category":"general_harms","explanation":"no"}`, 1))
	})
	if _, err := l.Generate(context.Background(), req()); !errors.Is(err, ErrRefused) {
		t.Fatalf("want ErrRefused, got %v", err)
	}
	if len(b.adds) != 1 {
		t.Fatal("a refused response is still billed and must be recorded")
	}
}

func TestLLM_WebResearchUsesAllowlistAndStrictTool(t *testing.T) {
	b := &memBudget{}
	l, _, bodies := newTestLLM(t, b, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"id":"msg_2","type":"message","role":"assistant","model":"claude-sonnet-5",
		 "content":[{"type":"tool_use","id":"tu_1","name":"submit_research","input":{"claims":[]}}],
		 "stop_reason":"tool_use","stop_sequence":null,
		 "usage":{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,
		          "server_tool_use":{"web_search_requests":3,"web_fetch_requests":1}}}`)
	})
	r := req()
	r.WebCategory, r.ToolName = CatFinance, "submit_research"
	res, err := l.Generate(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if string(res.JSON) != `{"claims":[]}` || res.Usage.WebSearches != 3 || math.Abs(b.adds[0]-0.03) > 1e-9 {
		t.Fatalf("json=%s usage=%+v adds=%v", res.JSON, res.Usage, b.adds)
	}
	body := (*bodies)[0]
	for _, want := range []string{`"web_search_20260209"`, `"web_fetch_20260209"`, `"allowed_domains"`, `"irs.gov"`, `"strict":true`, `"submit_research"`} {
		if !strings.Contains(body, want) {
			t.Errorf("request missing %s", want)
		}
	}
	if strings.Contains(body, `"output_config"`) {
		t.Error("web research returns through the strict tool, not output_config")
	}
}

func TestLLM_UnknownCategoryNeverSearchesTheOpenWeb(t *testing.T) {
	l, calls, _ := newTestLLM(t, &memBudget{}, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, msgText) })
	r := req()
	r.WebCategory, r.ToolName = "astrology", "submit_research"
	if _, err := l.Generate(context.Background(), r); !errors.Is(err, ErrNoAllowlist) || *calls != 0 {
		t.Fatalf("err=%v calls=%d", err, *calls)
	}
}

func TestLLM_PauseTurnResumes(t *testing.T) {
	var n int32
	l, calls, bodies := newTestLLM(t, &memBudget{}, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) == 1 {
			io.WriteString(w, strings.Replace(msgText, `"end_turn"`, `"pause_turn"`, 1))
			return
		}
		io.WriteString(w, msgText)
	})
	if _, err := l.Generate(context.Background(), req()); err != nil || *calls != 2 {
		t.Fatalf("err=%v calls=%d", err, *calls)
	}
	if !strings.Contains((*bodies)[1], `"role":"assistant"`) {
		t.Fatal("the resumed request must carry the paused assistant turn")
	}
}

func TestCostUSD(t *testing.T) {
	if v := CostUSD("claude-opus-5", 1_000_000, 0, 0, 1_000_000, 0); math.Abs(v-30) > 1e-9 {
		t.Fatalf("opus: %v", v)
	}
	if v := CostUSD("claude-haiku-4-5", 0, 1_000_000, 1_000_000, 0, 1000); math.Abs(v-(1.25+0.1+10)) > 1e-9 {
		t.Fatalf("haiku cache + 1k searches: %v", v)
	}
	if PriceFor("some-future-model") != ModelPrices["claude-opus-5"] {
		t.Fatal("unknown models must be priced at the most expensive rate")
	}
}
