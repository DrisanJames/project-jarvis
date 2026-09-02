package notify

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// render_golden_test.go pins the exact rendered text of the Slack message
// standard (docs/SLACK_MESSAGE_STANDARD.md, operator ruling 2026-09-01) and
// sweeps every notify.Message literal in the repo for compliance.
//
// No transport is touched: every assertion runs against Render/Fallback, and the
// one Deliver test uses a fake in-memory notifier.

const (
	markWarn  = "⚠️ WARN"
	markAlert = "\U0001F6A8 ALERT"
	markOK    = "✅ OK"
)

// --- golden: one message per tier -----------------------------------------

func TestGoldenPerTier(t *testing.T) {
	cases := []struct {
		name string
		msg  Message
		want string
	}{
		{
			name: "EVENT — the conversions reference message",
			msg: Message{
				Tier: TierEvent, Scope: ScopeConversion,
				Headline: "Sam's Club (EF 8241) · payout *$27.50*",
				Body:     "Email: jane@example.com\nOffer: Sam's Club (EF 8241)\nTransaction: 7f3a",
			},
			want: "Conversion · Sam's Club (EF 8241) · payout *$27.50*",
		},
		{
			name: "WARN",
			msg: Message{
				Tier: TierWarn, Scope: ScopeCron,
				Headline: "worker muted · `send-worker` · idle *72h0m0s*",
				Body:     "Mute after: 48h0m0s",
			},
			want: markWarn + " · Cron · worker muted · `send-worker` · idle *72h0m0s*",
		},
		{
			name: "ALERT",
			msg: Message{
				Tier: TierAlert, Scope: ScopeSend,
				Headline: "waves unlanded · *3* waves · 54,097 recipients",
				Body:     "Waves: `w1`, `w2`, `w3`",
			},
			want: markAlert + " · Send · waves unlanded · *3* waves · 54,097 recipients",
		},
		{
			name: "OK",
			msg:  Message{Tier: TierOK, Scope: ScopeSend, Headline: "queued backlog · cleared"},
			want: markOK + " · Send · queued backlog · cleared",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Fallback(tc.msg); got != tc.want {
				t.Fatalf("fallback\n got %q\nwant %q", got, tc.want)
			}
			if got := headlineOf(Render(tc.msg)); got != tc.want {
				t.Fatalf("rendered headline\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// --- golden: one message per scope ----------------------------------------

func TestGoldenPerScope(t *testing.T) {
	for _, scope := range ScopeVocabulary {
		t.Run(scope, func(t *testing.T) {
			m := Message{Tier: TierAlert, Scope: scope, Headline: "invariant breached · *1*"}
			want := markAlert + " · " + scope + " · invariant breached · *1*"
			if got := Fallback(m); got != want {
				t.Fatalf("\n got %q\nwant %q", got, want)
			}
		})
	}
}

// --- structure enforcement -------------------------------------------------

func TestUnknownScopePanicsUnderTest(t *testing.T) {
	if !strictStandard {
		t.Fatal("strictStandard must be true inside a go test binary")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("an off-vocabulary scope must panic under test")
		}
	}()
	_ = Fallback(Message{Tier: TierEvent, Scope: "Whatever", Headline: "x"})
}

func TestKnownAliasScopesDoNotPanic(t *testing.T) {
	for alias, want := range scopeAliases {
		if got := NormalizeScope(alias); got != want {
			t.Fatalf("NormalizeScope(%q) = %q, want %q", alias, got, want)
		}
	}
}

func TestBodyCappedAtSixLines(t *testing.T) {
	body := "l1\nl2\nl3\nl4\nl5\nl6\nl7\nl8"
	blocks := Render(Message{Tier: TierEvent, Scope: ScopeDrip, Headline: "h", Body: body})
	got := blocks[1].(map[string]any)["text"].(string)
	want := "l1\nl2\nl3\nl4\nl5\n…"
	if got != want {
		t.Fatalf("body\n got %q\nwant %q", got, want)
	}
	if n := len(strings.Split(got, "\n")); n != bodyLineLimit {
		t.Fatalf("body has %d lines, want %d", n, bodyLineLimit)
	}
}

func TestContextIsOneLine(t *testing.T) {
	blocks := Render(Message{Tier: TierEvent, Scope: ScopeBoard, Headline: "h", Context: "06:00 MT\nextra\nmore"})
	ctx := blocks[1].(map[string]any)["elements"].([]any)[0].(map[string]any)["text"].(string)
	if ctx != "06:00 MT" {
		t.Fatalf("context = %q, want one line", ctx)
	}
}

func TestBannedPhrasesStripped(t *testing.T) {
	m := Message{
		Tier: TierAlert, Scope: ScopeSend,
		Headline: "FYI queue drained successfully · *0*",
		Body:     "Just a heads up Rows: 0",
	}
	head := headlineOf(Render(m))
	body := Render(m)[1].(map[string]any)["text"].(string)
	for _, p := range BannedPhrases {
		if strings.Contains(strings.ToLower(head), p) {
			t.Fatalf("headline %q still contains banned phrase %q", head, p)
		}
		if strings.Contains(strings.ToLower(body), p) {
			t.Fatalf("body %q still contains banned phrase %q", body, p)
		}
	}
}

func headlineOf(blocks []any) string {
	return blocks[0].(map[string]any)["text"].(map[string]any)["text"].(string)
}

// --- repo sweep: every notify.Message literal ------------------------------

// TestEveryMessageLiteralIsOnStandard parses every non-test .go file under
// internal/ and cmd/ and asserts each notify.Message composite literal declares
// a vocabulary Scope and carries no banned phrase (and no "as of" Context — the
// standard forbids "as of" prose when the time is now).
func TestEveryMessageLiteralIsOnStandard(t *testing.T) {
	root := repoRoot(t)
	valid := map[string]bool{}
	for _, s := range ScopeVocabulary {
		valid[s] = true
		valid["Scope"+s] = true // the exported constant name, e.g. ScopeSend
	}
	for a := range scopeAliases {
		valid[a] = true
	}

	found := 0
	for _, dir := range []string{"internal", "cmd"} {
		base := filepath.Join(root, dir)
		err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			fset := token.NewFileSet()
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return fmt.Errorf("parse %s: %w", path, perr)
			}
			ast.Inspect(f, func(n ast.Node) bool {
				cl, ok := n.(*ast.CompositeLit)
				if !ok || !isNotifyMessage(cl.Type) {
					return true
				}
				found++
				where := fset.Position(cl.Pos()).String()
				checkMessageLiteral(t, where, cl)
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if found == 0 {
		t.Fatal("swept 0 notify.Message literals — the sweep is not looking where it thinks it is")
	}
	t.Logf("swept %d notify.Message literals", found)
}

func isNotifyMessage(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Message" {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "notify"
}

func checkMessageLiteral(t *testing.T, where string, cl *ast.CompositeLit) {
	t.Helper()
	sawScope := false
	for _, el := range cl.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok {
			continue
		}
		switch key.Name {
		case "Scope":
			sawScope = true
			if !scopeExprIsValid(kv.Value) {
				t.Errorf("%s: Message.Scope is not in the standard vocabulary (%s)", where, strings.Join(ScopeVocabulary, "|"))
			}
		case "Headline", "Body", "Context", "Action":
			for _, lit := range stringLiterals(kv.Value) {
				low := strings.ToLower(lit)
				for _, p := range BannedPhrases {
					if strings.Contains(low, p) {
						t.Errorf("%s: Message.%s contains banned phrase %q — see docs/SLACK_MESSAGE_STANDARD.md", where, key.Name, p)
					}
				}
				if key.Name == "Context" && strings.HasPrefix(strings.TrimSpace(low), "as of") {
					t.Errorf("%s: Message.Context starts with \"as of\" — omit the context line when the time is now", where)
				}
			}
		}
	}
	if !sawScope {
		t.Errorf("%s: notify.Message literal declares no Scope", where)
	}
}

// scopeExprIsValid accepts notify.ScopeX / ScopeX selectors and string literals
// that are in the vocabulary or the alias table.
func scopeExprIsValid(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.SelectorExpr:
		if id, ok := v.X.(*ast.Ident); ok && id.Name == "notify" {
			return scopeConstNames()[v.Sel.Name]
		}
		// A field access (e.g. s.scope on worker.SlackAlerter) is supplied at
		// runtime; NormalizeScope validates it on the way out, and the wiring in
		// cmd/server/main.go passes notify.Scope* constants.
		return true
	case *ast.Ident:
		return scopeConstNames()[v.Name]
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return false
		}
		s, err := strconv.Unquote(v.Value)
		if err != nil {
			return false
		}
		for _, w := range ScopeVocabulary {
			if s == w {
				return true
			}
		}
		_, ok := scopeAliases[strings.ToLower(s)]
		return ok
	}
	return false
}

func scopeConstNames() map[string]bool {
	m := make(map[string]bool, len(ScopeVocabulary))
	for _, s := range ScopeVocabulary {
		m["Scope"+s] = true
	}
	return m
}

func stringLiterals(e ast.Expr) []string {
	var out []string
	ast.Inspect(e, func(n ast.Node) bool {
		if bl, ok := n.(*ast.BasicLit); ok && bl.Kind == token.STRING {
			if s, err := strconv.Unquote(bl.Value); err == nil {
				out = append(out, s)
			}
		}
		return true
	})
	return out
}

// --- repo sweep: the pager files that compose text for SlackAlerter ---------

// pagerSources are the files whose alert strings reach Slack through
// worker.SlackAlerter (first line -> Headline, rest -> Body). Their literals are
// not notify.Message literals, so they get their own banned-phrase sweep.
var pagerSources = []string{
	"internal/worker/outbox_selfcheck.go",
	"internal/worker/storage_guard.go",
	"internal/worker/campaign_health_monitor.go",
	"internal/worker/worker_health.go",
	"internal/worker/partner_drip_digest.go",
	"internal/api/everflow_postback_handler.go",
}

func TestPagerSourcesHaveNoBannedPhrase(t *testing.T) {
	root := repoRoot(t)
	for _, rel := range pagerSources {
		path := filepath.Join(root, rel)
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			bl, ok := n.(*ast.BasicLit)
			if !ok || bl.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(bl.Value)
			if err != nil {
				return true
			}
			low := strings.ToLower(s)
			for _, p := range BannedPhrases {
				if strings.Contains(low, p) {
					t.Errorf("%s: string literal contains banned phrase %q — see docs/SLACK_MESSAGE_STANDARD.md",
						fset.Position(bl.Pos()), p)
				}
			}
			return true
		})
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	// test cwd is internal/notify
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root %s has no go.mod: %v", root, err)
	}
	return root
}
