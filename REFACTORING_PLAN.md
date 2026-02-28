# Upside-Down Codebase Refactoring Plan

## Current State Assessment

| Metric                        | Value           | Target        |
|-------------------------------|-----------------|---------------|
| Total Go LOC                  | 160,716         | ~100,000      |
| Largest file (handlers.go)    | 3,714 lines     | ≤500 lines    |
| Max functions/file            | 131             | ≤15           |
| Source files without tests    | 165 / 216 (76%) | ≤20%          |
| Packages with zero tests      | 14              | 0             |
| Files with raw SQL in handlers| ~20             | 0             |
| Interfaces used for DI        | ~2              | All deps      |

### Critical Anti-Patterns Found

1. **God handler files** — `handlers.go` (3714 LOC, 131 funcs), `mailing_advanced_handlers.go` (3772 LOC), `mailing_handlers_full.go` (3013 LOC)
2. **Raw SQL in HTTP handlers** — handlers execute `db.Query/Exec` directly instead of through a repository
3. **No service layer** — business logic lives in HTTP handlers, making it untestable without spinning up a web server
4. **Concrete dependencies everywhere** — no interfaces, impossible to mock
5. **Setter injection** — `Server` has 20+ `SetX()` methods instead of constructor injection
6. **Types dumped into mega-files** — `everflow/types.go` has 52 structs, `mailgun/types.go` has 46
7. **No package-level documentation** — no `doc.go` files, no godoc comments on packages
8. **Mixed concerns in packages** — `internal/api/` has 59 files covering 15+ unrelated domains

---

## Target Architecture

```
cmd/server/main.go          ← Composition root (wiring only)

internal/
├── domain/                  ← Pure business types, no imports
│   ├── campaign.go
│   ├── subscriber.go
│   ├── suppression.go
│   └── tracking.go
│
├── service/                 ← Business logic (pure functions + interfaces)
│   ├── campaign/
│   │   ├── service.go       ← CampaignService interface + impl
│   │   ├── service_test.go
│   │   └── repository.go   ← CampaignRepository interface
│   ├── tracking/
│   ├── suppression/
│   ├── warmup/
│   └── sending/
│
├── repository/              ← Database access (implements service interfaces)
│   ├── postgres/
│   │   ├── campaign.go      ← implements service/campaign.CampaignRepository
│   │   ├── subscriber.go
│   │   └── suppression.go
│   └── memory/              ← In-memory implementations for tests
│
├── handler/                 ← HTTP layer (thin: parse request → call service → write response)
│   ├── campaign.go          ← ≤15 funcs, one per endpoint
│   ├── subscriber.go
│   ├── tracking.go
│   └── middleware/
│
├── engine/                  ← PMTA conviction engine (already well-structured)
│   ├── agent/               ← Individual agent types
│   ├── orchestrator.go
│   └── executor.go
│
├── esp/                     ← ESP integrations (SparkPost, SES, Mailgun, etc.)
│   ├── sparkpost/
│   ├── ses/
│   ├── mailgun/
│   └── pmta/
│
├── worker/                  ← Background workers
│   ├── sender/
│   ├── scheduler/
│   └── recovery/
│
└── pkg/                     ← Shared utilities
    ├── httputil/
    ├── logger/
    └── crypto/
```

### Key Principles

1. **Handler functions do THREE things only**: parse request, call service, write response
2. **Service functions are pure**: they take typed inputs, return typed outputs + error. No `*http.Request`, no `http.ResponseWriter`
3. **Repository is the only place SQL lives**: one function = one query
4. **Every dependency is an interface**: services depend on repository interfaces, handlers depend on service interfaces
5. **Files ≤ 500 lines, ≤ 15 functions**: split by subdomain when exceeded
6. **Every public function has a godoc comment**: what it does, what it returns, when it fails

---

## Phased Execution

### Phase 0: Foundation (Week 1) — BLOCKING

Create the interface contracts and shared utilities that everything else depends on.

| Task | Owner | Files |
|------|-------|-------|
| Define `domain/` package with pure types | - | New: `internal/domain/*.go` |
| Define repository interfaces | - | New: `internal/service/*/repository.go` |
| Create `httputil` response helpers | - | New: `internal/pkg/httputil/response.go` |
| Create `doc.go` for every package | - | New: one per package |
| Set up `golangci-lint` config | - | New: `.golangci.yml` |

### Phase 1: Extract the Critical Path (Weeks 2-3) — AFFILIATE MAIL

Refactor the files that the affiliate mail sending pipeline touches.

| Task | From → To | Impact |
|------|-----------|--------|
| Extract `CampaignService` | `mailing_handlers_full.go` → `service/campaign/service.go` | Removes 800 LOC from handler |
| Extract `CampaignRepository` | `mailing_handlers_full.go` → `repository/postgres/campaign.go` | Removes all SQL from handler |
| Extract `TrackingService` | `mailing/tracking.go` → `service/tracking/service.go` | Already mostly clean |
| Extract `SuppressionService` | `suppression_service.go` → `service/suppression/service.go` | Removes 2189 LOC handler |
| Thin out `send_worker.go` | Keep worker loop, extract send logic to `service/sending/` | Testable send logic |
| Split `esp_adapters.go` | One file per ESP under `esp/{name}/sender.go` | 1114 LOC → 5 × ~200 LOC |

### Phase 2: Kill the God Files (Weeks 3-5)

| File | LOC | Plan |
|------|-----|------|
| `api/handlers.go` (3714, 131 funcs) | Split into 12 handler files by domain: `handler/dashboard.go`, `handler/metrics.go`, `handler/alerts.go`, `handler/chat.go`, `handler/kanban.go`, `handler/everflow.go`, `handler/ongage.go`, `handler/suggestions.go`, etc. |
| `api/mailing_advanced_handlers.go` (3772) | Split: `handler/webhook_esp.go` (SparkPost/SES/Mailgun webhooks), `handler/ab_testing.go`, `handler/automation.go`, `handler/analytics.go` |
| `api/mailing_handlers_full.go` (3013) | After Phase 1, this becomes a thin handler calling services. Split remaining into `handler/list.go`, `handler/subscriber.go`, `handler/campaign.go` |
| `api/suppression_service.go` (2189) | Extract to `service/suppression/` + `handler/suppression.go` |
| `api/jarvis_orchestrator.go` (2668) | Extract to `service/jarvis/orchestrator.go` |
| `api/campaign_builder.go` (2265) | Extract to `service/campaign/builder.go` |
| `everflow/collector.go` (2681) | Split: `esp/everflow/collector.go`, `esp/everflow/parser.go`, `esp/everflow/metrics.go` |
| `server.go` (918) | Replace 20+ setters with a single `ServerDeps` struct constructor |

### Phase 3: Interface Everything (Week 5-6)

Replace all concrete dependencies with interfaces. This is what makes the code testable.

```go
// BEFORE (untestable)
type Orchestrator struct {
    db        *sql.DB
    factory   *AgentFactory
    processor *SignalProcessor
    executor  *Executor
}

// AFTER (testable)
type Orchestrator struct {
    agents    AgentProvider
    signals   SignalSource
    executor  CommandExecutor
    decisions DecisionStore
}

type AgentProvider interface {
    GetAgents(ctx context.Context, isp ISP) ([]Agent, error)
}
type SignalSource interface {
    Subscribe(isp ISP) <-chan SignalSnapshot
}
type CommandExecutor interface {
    Execute(ctx context.Context, d Decision) error
}
type DecisionStore interface {
    Store(ctx context.Context, d Decision) error
    Recent(ctx context.Context, limit int) ([]Decision, error)
}
```

Key interfaces to define:

| Interface | Package | Methods |
|-----------|---------|---------|
| `CampaignRepository` | `service/campaign` | `Get`, `List`, `Create`, `Update`, `Delete`, `Enqueue` |
| `SubscriberRepository` | `service/subscriber` | `Get`, `ListBySegment`, `Create`, `UpdateStatus`, `BulkImport` |
| `SuppressionRepository` | `service/suppression` | `IsSuppressed`, `Suppress`, `Remove`, `ListByOrg`, `Sync` |
| `TrackingRepository` | `service/tracking` | `RecordOpen`, `RecordClick`, `RecordUnsubscribe` |
| `EmailSender` | `service/sending` | `Send(ctx, Message) (Result, error)` |
| `CommandExecutor` | `engine` | `Execute(ctx, Decision) error` |
| `SignalSource` | `engine` | `Subscribe(ISP) <-chan SignalSnapshot` |

### Phase 4: Test Coverage (Weeks 6-8)

Target: every service function has a unit test, every handler has an integration test.

| Package | Current Tests | Target |
|---------|--------------|--------|
| `internal/engine/` (25 files) | 0 tests | Unit tests for all agents, orchestrator, executor |
| `internal/api/` (59 files) | 1 test file | Handler tests using `httptest` + mocked services |
| `internal/worker/` (16 untested) | 6 test files | Unit tests for all workers |
| `internal/pmta/` (8 files) | 0 tests | Unit tests for parser, client, health |
| `internal/segmentation/` (4 files) | 0 tests | Unit tests for query builder, engine |

### Phase 5: Documentation & Developer Experience (Week 8-9)

| Task | Detail |
|------|--------|
| `doc.go` for every package | One-paragraph description of what the package does and when to use it |
| Godoc on all exported types | Every public struct, interface, and function |
| `ARCHITECTURE.md` | Dependency graph, data flow diagrams |
| `CONTRIBUTING.md` | How to add a new handler, service, repository |
| `Makefile` | `make test`, `make lint`, `make build`, `make migrate` |
| CI pipeline | `golangci-lint` + `go test -race` + coverage threshold (70%) |

---

## Commenting Standards

### Package comments (`doc.go`)
```go
// Package campaign implements the campaign management service layer.
// It handles campaign lifecycle (create, schedule, send, complete),
// queue management, and A/B testing logic.
//
// This package depends on repository interfaces defined within it
// and should never import from handler/ or directly from database/.
package campaign
```

### Interface comments
```go
// CampaignRepository defines the data access contract for campaigns.
// Implementations live in repository/postgres/ and repository/memory/.
type CampaignRepository interface {
    // Get returns a single campaign by ID. Returns ErrNotFound if missing.
    Get(ctx context.Context, orgID, id string) (*domain.Campaign, error)
    // ... 
}
```

### Function comments
```go
// EnqueueSubscribers builds the send queue for a campaign by resolving
// the target segment, applying suppression rules, and inserting queue
// items in batches of 5000. Returns the total number of subscribers
// enqueued. Fails if the campaign is not in "scheduled" status.
func (s *Service) EnqueueSubscribers(ctx context.Context, campaignID string) (int, error) {
```

### What NOT to comment
```go
// BAD — restating the code
// Get returns the campaign
func Get(id string) *Campaign {

// BAD — narrating the change
// Added this function to fix the tracking bug
func InjectPixel(html string) string {
```

---

## File Size Guidelines

| Category | Max LOC | Max Functions | Max Structs |
|----------|---------|---------------|-------------|
| Handler file | 500 | 15 | 3 (handler struct + req/resp) |
| Service file | 600 | 15 | 5 |
| Repository file | 400 | 12 | 2 (repo struct + options) |
| Types file | 300 | 0 | 15 |
| Test file | 800 | 20 | 5 (mocks) |
| Worker file | 500 | 12 | 3 |

---

## Migration Strategy

**Rule: Never break the build.** Every PR must compile and pass existing tests.

1. Create new package structure alongside old code
2. Move one function at a time: create new function in service/, update handler to call it, delete old code
3. Use type aliases during transition: `type Campaign = domain.Campaign`
4. Deprecate old files with `// Deprecated: use service/campaign instead` comments
5. Delete old files only after all callers are migrated

---

## Priority Order (What to do first)

1. **Phase 0** — Create `domain/`, interfaces, `httputil/` (unblocks everything)
2. **Phase 1** — Extract critical path services (affiliate mail pipeline)
3. **Phase 2** — Split the 5 worst god files (handlers.go, mailing_*.go, suppression_service.go)
4. **Phase 3** — Interface all dependencies in engine/
5. **Phase 4** — Test coverage push (engine/ and worker/ first)
6. **Phase 5** — Documentation, lint, CI
