package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/ignite/sparkpost-monitor/internal/api"
)

// =============================================================================
// env.manifest.json <-> code (REQ-092 DoD 2)
// =============================================================================
// There is no CI in this repo, so this test IS the CI for the deploy manifest.
// It enforces the two halves of env-as-code:
//
//   1. every env var the send path reads is DECLARED (no undocumented lever)
//   2. every declared key names a READER (no fossil that nobody can retire)
//
// Both failures shipped: KAFKA_SEND_QUEUE_ALL decided a 90-minute zero-send on
// 2026-09-01 with no diff and no reviewer, while SUPPRESSION_S3_* has been
// upserted on every deploy since with zero readers in Go.

type manifestEntry struct {
	Name        string  `json:"name"`
	Value       *string `json:"value"`
	Class       string  `json:"class"`
	Reader      string  `json:"reader"`
	Since       string  `json:"since"`
	Note        string  `json:"note"`
	Default     bool    `json:"default"`
	Optional    bool    `json:"optional"`
	Passthrough bool    `json:"passthrough"`
	Carry       bool    `json:"carry"`
	AbsentOK    bool    `json:"absent_ok"`
}

type envManifest struct {
	Container string          `json:"container"`
	Env       []manifestEntry `json:"env"`
}

// repoRoot walks up from the test's working directory (cmd/server) to the
// module root, so the test is runnable from anywhere `go test` is invoked.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not locate go.mod above the working directory")
	return ""
}

func loadEnvManifest(t *testing.T) (envManifest, map[string]manifestEntry) {
	t.Helper()
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "deploy", "env.manifest.json"))
	if err != nil {
		t.Fatalf("read deploy/env.manifest.json: %v", err)
	}
	var m envManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse deploy/env.manifest.json: %v", err)
	}
	if len(m.Env) == 0 {
		t.Fatal("manifest declares no env")
	}
	byName := make(map[string]manifestEntry, len(m.Env))
	for _, e := range m.Env {
		if _, dup := byName[e.Name]; dup {
			t.Fatalf("duplicate manifest entry %q", e.Name)
		}
		byName[e.Name] = e
	}
	return m, byName
}

var envLiteralRe = regexp.MustCompile(`(?:os\.)?(?:Getenv|LookupEnv)\("([A-Z][A-Z0-9_]*)"\)`)

// envLiteralsIn collects os.Getenv / os.LookupEnv string literals under a file
// or directory, skipping _test.go files (their fixture vars are not deploy
// config).
func envLiteralsIn(t *testing.T, root, target string) map[string]string {
	t.Helper()
	found := map[string]string{}
	abs := filepath.Join(root, target)
	walk := func(path string, info os.FileInfo) {
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		rel, _ := filepath.Rel(root, path)
		for _, m := range envLiteralRe.FindAllStringSubmatch(string(raw), -1) {
			if _, seen := found[m[1]]; !seen {
				found[m[1]] = rel
			}
		}
	}
	info, err := os.Stat(abs)
	if err != nil {
		t.Fatalf("stat %s: %v", target, err)
	}
	if info.IsDir() {
		err = filepath.Walk(abs, func(path string, fi os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if !fi.IsDir() {
				walk(path, fi)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", target, err)
		}
	} else {
		walk(abs, info)
	}
	return found
}

// TestEnvManifestDeclaresSendPathEnv: every env literal the send transport
// reads must be declared in the manifest.
func TestEnvManifestDeclaresSendPathEnv(t *testing.T) {
	root := repoRoot(t)
	_, byName := loadEnvManifest(t)

	// HOSTNAME is injected by the container runtime, never by the task
	// definition; it is declared in the manifest as a documented default.
	targets := []string{
		"internal/eventbus/sendqueue",
		"internal/worker/kafka_send_route.go",
		"cmd/server/eventbus_wiring.go",
	}
	var missing []string
	for _, target := range targets {
		for name, where := range envLiteralsIn(t, root, target) {
			if _, ok := byName[name]; !ok {
				missing = append(missing, name+" ("+where+")")
			}
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("send-path env read in code but NOT declared in deploy/env.manifest.json:\n  %s\n"+
			"Add each with a class, a reader and a note — an undeclared lever is a "+
			"flag flip with no diff and no reviewer.", strings.Join(missing, "\n  "))
	}
}

// TestEnvManifestDeclaresDisableFamily: the DISABLE_* kill-switch family is the
// set of levers that can silently change send behaviour, so all of it is
// declared — including the ones deliberately left unset (class default).
func TestEnvManifestDeclaresDisableFamily(t *testing.T) {
	root := repoRoot(t)
	_, byName := loadEnvManifest(t)

	found := map[string]string{}
	for _, dir := range []string{"cmd", "internal"} {
		for name, where := range envLiteralsIn(t, root, dir) {
			if strings.HasPrefix(name, "DISABLE_") {
				found[name] = where
			}
		}
	}
	if len(found) == 0 {
		t.Fatal("no DISABLE_* env literals found — the scan is broken, not the code")
	}
	var missing []string
	for name, where := range found {
		if _, ok := byName[name]; !ok {
			missing = append(missing, name+" ("+where+")")
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("DISABLE_* kill switches read in code but NOT declared in deploy/env.manifest.json:\n  %s",
			strings.Join(missing, "\n  "))
	}
}

// TestEnvManifestEveryKeyHasReader: the inverse direction — a declared key with
// no reader is a fossil that gets upserted on every deploy forever (the
// SUPPRESSION_S3_* shape). class "removed" is exempt: no reader is the reason
// it is being removed.
func TestEnvManifestEveryKeyHasReader(t *testing.T) {
	_, byName := loadEnvManifest(t)
	var bad []string
	for name, e := range byName {
		if e.Class == "removed" {
			continue
		}
		if strings.TrimSpace(e.Reader) == "" {
			bad = append(bad, name)
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Errorf("manifest keys with no reader (file:line) — either find the reader "+
			"or set class \"removed\":\n  %s", strings.Join(bad, "\n  "))
	}
}

// TestEnvManifestReadersResolve: a reader is only evidence if the file exists
// and still mentions the name. This catches the stale-pointer failure mode
// where a var is renamed and the manifest keeps naming a dead line.
func TestEnvManifestReadersResolve(t *testing.T) {
	root := repoRoot(t)
	_, byName := loadEnvManifest(t)
	var bad []string
	for name, e := range byName {
		if e.Class == "removed" || strings.TrimSpace(e.Reader) == "" {
			continue
		}
		path := e.Reader
		if i := strings.LastIndex(path, ":"); i >= 0 {
			path = path[:i]
		}
		full := filepath.Join(root, path)
		info, err := os.Stat(full)
		if err != nil {
			bad = append(bad, name+" -> "+e.Reader+" (no such file)")
			continue
		}
		if info.IsDir() {
			bad = append(bad, name+" -> "+e.Reader+" (is a directory)")
			continue
		}
		raw, err := os.ReadFile(full)
		if err != nil {
			bad = append(bad, name+" -> "+e.Reader+" (unreadable)")
			continue
		}
		// The name may be composed (FlagGate builds KAFKA_FLAG_<NAME>) or bound
		// to an exported const, so accept the name OR a documented suffix.
		body := string(raw)
		if strings.Contains(body, name) {
			continue
		}
		if strings.HasPrefix(name, "KAFKA_FLAG_") && strings.Contains(body, "KAFKA_FLAG_") {
			continue
		}
		bad = append(bad, name+" -> "+e.Reader+" (file does not mention the name)")
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Errorf("manifest readers that do not resolve:\n  %s", strings.Join(bad, "\n  "))
	}
}

// TestEnvManifestClassesAndValues pins the invariants prepare_task_definition.py
// relies on, so a bad hand-edit fails here rather than at register time.
func TestEnvManifestClassesAndValues(t *testing.T) {
	m, _ := loadEnvManifest(t)
	valid := map[string]bool{
		"kill": true, "route": true, "capacity": true,
		"infra": true, "secret": true, "stamp": true, "removed": true,
	}
	for _, e := range m.Env {
		if !valid[e.Class] {
			t.Errorf("%s: invalid class %q", e.Name, e.Class)
		}
		// A secret's value must never be committed.
		if e.Class == "secret" && e.Value != nil {
			t.Errorf("%s: class secret must have value null — no secret goes in git", e.Name)
		}
		// Deploy stamps are written by prepare_task_definition.py from argv.
		if e.Class == "stamp" && e.Value != nil {
			t.Errorf("%s: class stamp must have value null (written at register time)", e.Name)
		}
		// A removed key must not carry a value to upsert.
		if e.Class == "removed" && e.Value != nil {
			t.Errorf("%s: class removed must have value null", e.Name)
		}
		// "default" means deliberately unset; a value contradicts that.
		if e.Default && e.Value != nil {
			t.Errorf("%s: default:true and a value are contradictory", e.Name)
		}
		// Passthrough only applies to unmanaged values; a manifest value wins.
		if e.Passthrough && e.Value != nil {
			t.Errorf("%s: passthrough:true with a manifest value — the shell would "+
				"silently override git", e.Name)
		}
	}
	if m.Container == "" {
		t.Error("manifest has no container name")
	}
}

// TestEnvManifestCoversTaskDefinitionStamps: the two new provenance stamps must
// be declared, or prepare_task_definition.py would write env the manifest gate
// then rejects on the NEXT deploy.
func TestEnvManifestCoversTaskDefinitionStamps(t *testing.T) {
	_, byName := loadEnvManifest(t)
	for _, name := range []string{
		"APP_VERSION", "APP_GIT_SHA", "APP_BUILD_TIME", "APP_IMAGE_URI",
		"APP_IMAGE_DIGEST", "APP_ENV_MANIFEST_SHA", "APP_TREE_DIRTY",
	} {
		e, ok := byName[name]
		if !ok {
			t.Errorf("deploy stamp %s is not declared in the manifest", name)
			continue
		}
		if e.Class != "stamp" {
			t.Errorf("%s: expected class stamp, got %q", name, e.Class)
		}
	}
}

// TestEnvManifestHealthWiring proves the two new /health surfaces are actually
// reachable, not just defined: the deploy script's post-stability gate reads
// build.env_manifest_sha, and REQ-094 reads migrations.failed. A field that
// compiles but never reaches the JSON is this repo's most-shipped bug.
func TestEnvManifestHealthWiring(t *testing.T) {
	t.Setenv("APP_ENV_MANIFEST_SHA", "deadbeef")
	t.Setenv("APP_TREE_DIRTY", "1")

	publishMigrationsStatus(api.MigrationsStatus{
		Ran: true, OK: 3, Skipped: 1, Timeout: 2, Failed: 1,
		FailedNames: []string{"ensure_tracking_email_col (timeout)"},
		DurationMS:  1234,
	})

	rec := httptest.NewRecorder()
	api.NewHealthChecker(nil, nil, nil, "").
		HandleHealth(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/health returned %d", rec.Code)
	}

	var payload struct {
		Build struct {
			EnvManifestSHA string `json:"env_manifest_sha"`
			TreeDirty      string `json:"tree_dirty"`
		} `json:"build"`
		Migrations api.MigrationsStatus `json:"migrations"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode /health: %v", err)
	}
	if payload.Build.EnvManifestSHA != "deadbeef" {
		t.Errorf("build.env_manifest_sha = %q, want %q", payload.Build.EnvManifestSHA, "deadbeef")
	}
	if payload.Build.TreeDirty != "1" {
		t.Errorf("build.tree_dirty = %q, want %q", payload.Build.TreeDirty, "1")
	}
	if !payload.Migrations.Ran || payload.Migrations.Failed != 1 || payload.Migrations.Timeout != 2 {
		t.Errorf("migrations = %+v, want ran=true failed=1 timeout=2", payload.Migrations)
	}
	if len(payload.Migrations.FailedNames) != 1 {
		t.Errorf("migrations.failed_names = %v, want 1 entry", payload.Migrations.FailedNames)
	}
}
