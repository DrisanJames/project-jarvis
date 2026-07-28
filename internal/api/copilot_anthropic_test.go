package api

// Tests for the Copilot's Anthropic provider path (copilot_anthropic.go) and
// the scheduler-bridge whitelist (copilot_bridge_tools.go).
//
// Pattern follows click_drip_admin_handlers_test.go: go-sqlmock (regex
// matcher) + httptest. The Anthropic API is stubbed with an httptest server
// via CampaignCopilot.anthropicBaseURL.
//
// Coverage:
//   - resolveCopilotModel routing table (fable default, gpt-* passthrough,
//     unknown → error listing the allowed set)
//   - HandleChat 400 on unknown model
//   - HandleChat defaults to claude-fable-5 when ANTHROPIC_API_KEY is set
//   - one full tool_use → tool_result round-trip against the stub
//   - queue_scheduler_command whitelist rejection (pre-DB) + happy INSERT

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestCopilot(t *testing.T) (*CampaignCopilot, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(
		sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp),
		sqlmock.MonitorPingsOption(false),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mock.MatchExpectationsInOrder(false)
	return &CampaignCopilot{
		db:                  db,
		openAIKey:           "sk-test-openai",
		model:               "gpt-4.1",
		httpClient:          &http.Client{Timeout: 5 * time.Second},
		anthropicKey:        "sk-ant-test",
		anthropicHTTPClient: &http.Client{Timeout: 5 * time.Second},
	}, mock
}

func copilotChatPOST(t *testing.T, c *CampaignCopilot, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/mailing/copilot/chat", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	c.HandleChat(w, req)
	return w
}

// ─── model routing ───────────────────────────────────────────────────────────

func TestCopilotResolveModel(t *testing.T) {
	c, _ := newTestCopilot(t)

	provider, model, errMsg := c.resolveCopilotModel("")
	assert.Equal(t, "anthropic", provider)
	assert.Equal(t, "claude-fable-5", model)
	assert.Empty(t, errMsg)

	provider, model, errMsg = c.resolveCopilotModel("claude-opus-5")
	assert.Equal(t, "anthropic", provider)
	assert.Equal(t, "claude-opus-5", model)
	assert.Empty(t, errMsg)

	provider, model, errMsg = c.resolveCopilotModel("gpt-4.1")
	assert.Equal(t, "openai", provider)
	assert.Equal(t, "gpt-4.1", model)
	assert.Empty(t, errMsg)

	_, _, errMsg = c.resolveCopilotModel("claude-sonnet-4-6")
	assert.Contains(t, errMsg, "claude-fable-5")
	assert.Contains(t, errMsg, "claude-opus-5")

	// Without an Anthropic key, empty model stays on the OpenAI path.
	c.anthropicKey = ""
	provider, model, errMsg = c.resolveCopilotModel("")
	assert.Equal(t, "openai", provider)
	assert.Equal(t, "gpt-4.1", model)
	assert.Empty(t, errMsg)
}

func TestCopilotChatUnknownModelReturns400(t *testing.T) {
	c, _ := newTestCopilot(t)

	w := copilotChatPOST(t, c, `{"message":"hello","model":"llama-3"}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "claude-fable-5")
	assert.Contains(t, w.Body.String(), "claude-opus-5")
}

// ─── anthropic stub helpers ──────────────────────────────────────────────────

type stubAnthropicCall struct {
	Model    string
	RawBody  string
	Messages []json.RawMessage
}

// newAnthropicStub serves canned Messages-API responses in order and records
// each request.
func newAnthropicStub(t *testing.T, responses []string) (*httptest.Server, *[]stubAnthropicCall) {
	t.Helper()
	calls := &[]stubAnthropicCall{}
	i := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/messages", r.URL.Path)
		require.Equal(t, "sk-ant-test", r.Header.Get("x-api-key"))
		require.Equal(t, anthropicVersion, r.Header.Get("anthropic-version"))

		var body struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		raw := new(strings.Builder)
		dec := json.NewDecoder(io_TeeReader(r, raw))
		require.NoError(t, dec.Decode(&body))
		msgs := make([]json.RawMessage, 0, len(body.Messages))
		for _, m := range body.Messages {
			msgs = append(msgs, m.Content)
		}
		*calls = append(*calls, stubAnthropicCall{Model: body.Model, RawBody: raw.String(), Messages: msgs})

		resp := responses[min(i, len(responses)-1)]
		i++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(resp))
	}))
	t.Cleanup(srv.Close)
	return srv, calls
}

// io_TeeReader mirrors the request body into a builder without an io import
// dance in each test.
func io_TeeReader(r *http.Request, sb *strings.Builder) *teeReader {
	return &teeReader{r: r, sb: sb}
}

type teeReader struct {
	r  *http.Request
	sb *strings.Builder
}

func (t *teeReader) Read(p []byte) (int, error) {
	n, err := t.r.Body.Read(p)
	if n > 0 {
		t.sb.Write(p[:n])
	}
	return n, err
}

func TestCopilotChatDefaultsToFable(t *testing.T) {
	c, _ := newTestCopilot(t)

	srv, calls := newAnthropicStub(t, []string{
		`{"type":"message","model":"claude-fable-5","stop_reason":"end_turn",
		  "content":[{"type":"text","text":"Board looks healthy."}]}`,
	})
	c.anthropicBaseURL = srv.URL

	w := copilotChatPOST(t, c, `{"message":"how is the board?"}`)
	require.Equal(t, http.StatusOK, w.Code)

	require.Len(t, *calls, 1)
	assert.Equal(t, "claude-fable-5", (*calls)[0].Model)

	var resp copilotChatResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "Board looks healthy.", resp.Response)
	assert.Equal(t, "claude-fable-5", resp.Model)
	assert.True(t, resp.AIPowered)
}

// One full tool round-trip: the stub asks for queue_scheduler_command with a
// non-whitelisted command; the dispatcher must reject it WITHOUT touching the
// DB, the loop must send the error back as a tool_result, and the second
// response closes the turn.
func TestCopilotAnthropicToolUseLoop(t *testing.T) {
	c, mock := newTestCopilot(t)

	srv, calls := newAnthropicStub(t, []string{
		`{"type":"message","model":"claude-opus-5","stop_reason":"tool_use",
		  "content":[
		    {"type":"text","text":"Queueing that now."},
		    {"type":"tool_use","id":"toolu_01","name":"queue_scheduler_command",
		     "input":{"command":"rm-rf-everything","args":{}}}
		  ]}`,
		`{"type":"message","model":"claude-opus-5","stop_reason":"end_turn",
		  "content":[{"type":"text","text":"That command is not allowed."}]}`,
	})
	c.anthropicBaseURL = srv.URL

	w := copilotChatPOST(t, c, `{"message":"nuke it","model":"claude-opus-5"}`)
	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, *calls, 2)

	// Second request must carry the echoed assistant turn + a tool_result
	// answering toolu_01 with the whitelist rejection.
	second := (*calls)[1].RawBody
	assert.Contains(t, second, `"tool_use"`)
	assert.Contains(t, second, `"tool_result"`)
	assert.Contains(t, second, "toolu_01")
	assert.Contains(t, second, "not allowed")
	assert.Contains(t, second, "build-send-day")

	var resp copilotChatResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "That command is not allowed.", resp.Response)

	// The whitelist rejection never reached the database.
	assert.NoError(t, mock.ExpectationsWereMet())
}

// ─── queue_scheduler_command whitelist ───────────────────────────────────────

func TestQueueSchedulerCommandWhitelist(t *testing.T) {
	c, mock := newTestCopilot(t)
	ctx := context.Background()

	// Rejected commands never hit the DB.
	for _, bad := range []string{"", "rm -rf /", "deploy", "build-send-day; drop table"} {
		result, action := c.toolQueueSchedulerCommand(ctx, "org-1", map[string]interface{}{"command": bad})
		m, ok := result.(map[string]string)
		require.True(t, ok, "command %q should return an error map", bad)
		assert.Contains(t, m["error"], "not allowed")
		assert.Contains(t, m["error"], "build-send-day")
		assert.Empty(t, action)
	}
	assert.NoError(t, mock.ExpectationsWereMet())

	// Whitelisted command inserts with requested_by carrying the model.
	mock.ExpectQuery(`INSERT INTO mailing_scheduler_commands`).
		WithArgs("org-1", "write-directive", []byte(`{"date":"2026-07-29"}`), "copilot:claude-fable-5").
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).
			AddRow("cmd-uuid-1", time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)))

	mctx := context.WithValue(ctx, copilotModelCtxKey, "claude-fable-5")
	result, action := c.toolQueueSchedulerCommand(mctx, "org-1", map[string]interface{}{
		"command": "write-directive",
		"args":    map[string]interface{}{"date": "2026-07-29"},
	})
	row, ok := result.(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "cmd-uuid-1", row["id"])
	assert.Equal(t, "queued", row["status"])
	assert.Equal(t, "Queued scheduler command: write-directive", action)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestBoardPrefixForDate(t *testing.T) {
	p, err := boardPrefixForDate("2026-07-29")
	require.NoError(t, err)
	assert.Equal(t, "jul29", p)

	p, err = boardPrefixForDate("2026-07-02")
	require.NoError(t, err)
	assert.Equal(t, "jul02", p)

	p, err = boardPrefixForDate("jul29")
	require.NoError(t, err)
	assert.Equal(t, "jul29", p)

	_, err = boardPrefixForDate("tomorrow")
	assert.Error(t, err)
}
