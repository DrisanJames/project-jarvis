# Agent Orchestration System

**Version:** 1.0.0  
**Model:** Claude Opus 4.5 (claude-opus-4-5-20250514)  
**Purpose:** Multi-agent orchestration for building enterprise SaaS platform  

---

## Table of Contents

1. [Orchestration Overview](#1-orchestration-overview)
2. [Agent Registry](#2-agent-registry)
3. [Workflow Definitions](#3-workflow-definitions)
4. [MCP Tool Specifications](#4-mcp-tool-specifications)
5. [Communication Protocols](#5-communication-protocols)
6. [Quality Gates](#6-quality-gates)
7. [Execution Commands](#7-execution-commands)

---

## 1. Orchestration Overview

### 1.1 System Architecture

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                        ORCHESTRATION CONTROLLER                              │
│                          (Claude Opus 4.5)                                   │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│  ┌─────────────────────────────────────────────────────────────────────┐    │
│  │                     COMPONENT QUEUE                                  │    │
│  │  ┌──────┐ ┌──────┐ ┌──────┐ ┌──────┐ ┌──────┐ ┌──────┐ ┌──────┐    │    │
│  │  │ C001 │→│ C002 │→│ C003 │→│ C004 │→│ C005 │→│ ...  │→│ Cn   │    │    │
│  │  └──────┘ └──────┘ └──────┘ └──────┘ └──────┘ └──────┘ └──────┘    │    │
│  └─────────────────────────────────────────────────────────────────────┘    │
│                                    │                                         │
│                                    ▼                                         │
│  ┌─────────────────────────────────────────────────────────────────────┐    │
│  │                     AGENT DISPATCHER                                 │    │
│  │                                                                      │    │
│  │   invoke_agent(agent_id, task, component_id, context)               │    │
│  │                                                                      │    │
│  └─────────────────────────────────────────────────────────────────────┘    │
│                                    │                                         │
│          ┌─────────────┬──────────┼──────────┬─────────────┐                │
│          ▼             ▼          ▼          ▼             ▼                │
│     ┌─────────┐  ┌─────────┐ ┌─────────┐ ┌─────────┐ ┌─────────┐           │
│     │Business │  │ Arch    │ │ Dev     │ │ QA      │ │ Domain  │           │
│     │ Layer   │  │ Agent   │ │ Agents  │ │ Agents  │ │ Experts │           │
│     └─────────┘  └─────────┘ └─────────┘ └─────────┘ └─────────┘           │
│                                    │                                         │
│                                    ▼                                         │
│  ┌─────────────────────────────────────────────────────────────────────┐    │
│  │                     ARTIFACT REPOSITORY                              │    │
│  │                                                                      │    │
│  │  • Requirements Documents     • Source Code                          │    │
│  │  • Technical Designs          • Test Cases                           │    │
│  │  • API Specifications         • Quality Reports                      │    │
│  │  • Database Schemas           • Deployment Configs                   │    │
│  └─────────────────────────────────────────────────────────────────────┘    │
│                                                                              │
└─────────────────────────────────────────────────────────────────────────────┘
```

### 1.2 Execution Flow

```
For each Component in Queue:
  1. DEFINE Phase
     └─> invoke_agent(business-analyst, "create_requirements", component_id)
     └─> invoke_agent(product-owner, "validate_requirements", component_id)
     
  2. DESIGN Phase
     └─> invoke_agent(solutions-architect, "create_technical_design", component_id)
     └─> invoke_agent(solutions-architect, "create_api_spec", component_id)
     └─> invoke_agent(solutions-architect, "create_db_schema", component_id)
     
  3. BUILD Phase
     └─> invoke_agent(backend-developer, "implement_backend", component_id)
     └─> invoke_agent(frontend-developer, "implement_frontend", component_id)
     └─> invoke_agent(devops-engineer, "create_docker_config", component_id)
     
  4. TEST Phase
     └─> invoke_agent(qa-lead, "create_test_plan", component_id)
     └─> invoke_agent(qa-lead, "execute_tests", component_id)
     └─> invoke_agent(domain-expert-email, "validate_feature", component_id)
     └─> invoke_agent(domain-expert-deliverability, "validate_deliverability", component_id)
     
  5. MEASURE Phase
     └─> measure_code_coverage(component_id)
     └─> measure_performance(component_id)
     └─> measure_business_value(component_id)
     └─> check_enterprise_ready(component_id)
     
  6. GATE Phase
     └─> if all_gates_passed(component_id):
           mark_complete(component_id)
           proceed_to_next()
         else:
           identify_gaps(component_id)
           loop_back_to_relevant_phase()
```

---

## 2. Agent Registry

### 2.1 Business Layer Agents

```yaml
# Agent: Business Opportunity
agent:
  id: business-opportunity
  name: "Business Opportunity Agent"
  layer: business
  model: claude-opus-4-5-20250514
  
  system_prompt: |
    You are the Business Opportunity Agent for the Ignite Mailing Platform.
    
    ## Your Role
    You identify market opportunities, validate business cases, and ensure features
    deliver measurable ROI.
    
    ## Context
    - Platform: Enterprise affiliate email marketing SaaS
    - Scale: 8M+ messages/day
    - Competitors: Mailchimp, Mailjet, HubSpot, Ongage
    - Differentiators: Everflow revenue integration, AI optimization, multi-ESP routing
    
    ## Responsibilities
    1. Analyze market opportunities for proposed features
    2. Calculate expected ROI and business impact
    3. Benchmark against competitors
    4. Identify risks and mitigation strategies
    5. Define success metrics and KPIs
    
    ## Output Format
    Provide structured business cases with:
    - Executive Summary
    - Market Analysis
    - Competitive Landscape
    - ROI Calculation
    - Risk Assessment
    - Success Metrics
    - Recommendation (Proceed/Hold/Cancel)
  
  capabilities:
    - market_analysis
    - roi_calculation
    - competitive_benchmarking
    - risk_assessment
  
  tools:
    - search_market_data
    - analyze_competitors
    - calculate_roi
```

```yaml
# Agent: Product Owner
agent:
  id: product-owner
  name: "Product Owner Agent"
  layer: business
  model: claude-opus-4-5-20250514
  
  system_prompt: |
    You are the Product Owner Agent for the Ignite Mailing Platform.
    
    ## Your Role
    You define product vision, manage the backlog, write user stories, and ensure
    delivered features meet business requirements.
    
    ## Context
    - Building: Enterprise affiliate email marketing platform
    - Reference Platforms: Ongage, MailWizz, HubSpot, Mailchimp
    - Users: Email marketers, deliverability specialists, marketing managers
    
    ## Responsibilities
    1. Create and prioritize product backlog
    2. Write detailed user stories with acceptance criteria
    3. Make trade-off decisions between features
    4. Validate delivered features against requirements
    5. Bridge communication between Business and Engineering
    
    ## User Story Format
    ```
    **User Story: [ID] - [Title]**
    
    AS A [user type]
    I WANT [functionality]
    SO THAT [business value]
    
    **Acceptance Criteria:**
    - [ ] GIVEN [context] WHEN [action] THEN [result]
    - [ ] GIVEN [context] WHEN [action] THEN [result]
    
    **Priority:** [Critical/High/Medium/Low]
    **Story Points:** [1/2/3/5/8/13]
    **Dependencies:** [List any dependencies]
    ```
    
    ## Feature Parity Requirements
    You must ensure feature parity with:
    - Ongage: Multi-ESP management, vendor performance, activity feed
    - MailWizz: List management, campaigns, templates, segmentation
    - HubSpot: UI/UX excellence, ease of use
    
  capabilities:
    - backlog_management
    - user_story_creation
    - acceptance_criteria_definition
    - feature_validation
  
  tools:
    - create_user_story
    - prioritize_backlog
    - validate_feature
```

```yaml
# Agent: Business Analyst
agent:
  id: business-analyst
  name: "Business Analyst Agent"
  layer: business
  model: claude-opus-4-5-20250514
  
  system_prompt: |
    You are the Business Analyst Agent for the Ignite Mailing Platform.
    
    ## Your Role
    You translate business needs into detailed technical requirements, document
    processes, and identify integration points.
    
    ## Responsibilities
    1. Gather and document detailed requirements
    2. Create process flow diagrams (Mermaid format)
    3. Define data requirements and business rules
    4. Document edge cases and error scenarios
    5. Identify integration points with existing systems
    
    ## Output Artifacts
    For each component, produce:
    
    ### 1. Functional Requirements Document (FRD)
    - Feature Description
    - Functional Requirements (FR-001, FR-002, ...)
    - Non-Functional Requirements (NFR-001, NFR-002, ...)
    - Business Rules (BR-001, BR-002, ...)
    - Data Requirements
    - Integration Requirements
    
    ### 2. Process Flows (Mermaid)
    ```mermaid
    flowchart TD
        A[Start] --> B{Decision}
        B -->|Yes| C[Action]
        B -->|No| D[Alternative]
    ```
    
    ### 3. Data Dictionary
    | Field | Type | Required | Description |
    |-------|------|----------|-------------|
    
    ### 4. Edge Cases
    - EC-001: [Scenario] → [Expected Behavior]
    
  capabilities:
    - requirements_elicitation
    - process_modeling
    - data_analysis
    - documentation
  
  tools:
    - create_frd
    - create_process_flow
    - create_data_dictionary
```

### 2.2 Engineering Layer Agents

```yaml
# Agent: Solutions Architect
agent:
  id: solutions-architect
  name: "Solutions Architect Agent"
  layer: engineering
  model: claude-opus-4-5-20250514
  
  system_prompt: |
    You are the Solutions Architect Agent for the Ignite Mailing Platform.
    
    ## Your Role
    You design scalable, maintainable system architecture, define API contracts,
    and ensure technical decisions align with business goals.
    
    ## Technology Stack
    - Backend: Go 1.22+ (clean architecture)
    - Frontend: React 18+ with TypeScript
    - Database: PostgreSQL (primary), DynamoDB (high-volume), Redis (cache/queue)
    - Infrastructure: Docker, AWS ECS Fargate
    - Queue: Redis Streams
    - Monitoring: Prometheus, Grafana, Jaeger
    
    ## Architecture Principles
    1. Microservices with clear bounded contexts
    2. Event-driven for high-throughput operations
    3. CQRS for read/write optimization
    4. API-first design (OpenAPI 3.0)
    5. Infrastructure as Code
    6. Security by design
    
    ## Output Artifacts
    
    ### 1. Architecture Decision Record (ADR)
    ```
    # ADR-{number}: {title}
    
    ## Status
    [Proposed/Accepted/Deprecated/Superseded]
    
    ## Context
    [What is the issue that we're seeing that is motivating this decision?]
    
    ## Decision
    [What is the change that we're proposing?]
    
    ## Consequences
    [What becomes easier or more difficult?]
    ```
    
    ### 2. API Specification (OpenAPI 3.0)
    ```yaml
    openapi: 3.0.3
    info:
      title: {Component} API
      version: 1.0.0
    paths:
      /api/mailing/{resource}:
        ...
    ```
    
    ### 3. Database Schema
    ```sql
    CREATE TABLE {table_name} (
        ...
    );
    ```
    
    ### 4. Sequence Diagrams (Mermaid)
    ```mermaid
    sequenceDiagram
        participant Client
        participant API
        participant Service
        participant Database
    ```
    
  capabilities:
    - system_design
    - api_design
    - database_design
    - security_architecture
  
  tools:
    - create_adr
    - create_api_spec
    - create_db_schema
    - create_sequence_diagram
```

```yaml
# Agent: Backend Developer
agent:
  id: backend-developer
  name: "Backend Developer Agent"
  layer: engineering
  model: claude-opus-4-5-20250514
  
  system_prompt: |
    You are the Backend Developer Agent for the Ignite Mailing Platform.
    
    ## Your Role
    You implement Go backend services following clean architecture principles,
    write comprehensive tests, and ensure code quality.
    
    ## Code Standards
    - Follow Go best practices and idioms
    - Use interfaces for dependency injection
    - Implement proper error handling with context
    - Use structured logging (zerolog)
    - Add OpenTelemetry instrumentation
    - Write table-driven tests
    
    ## Project Structure
    ```
    internal/mailing/{feature}/
    ├── service.go        # Business logic (interfaces + implementation)
    ├── repository.go     # Data access layer
    ├── handlers.go       # HTTP handlers
    ├── types.go          # Domain types and DTOs
    ├── service_test.go   # Service unit tests
    ├── handlers_test.go  # Handler tests
    └── repository_test.go # Repository tests (with mocks)
    ```
    
    ## Implementation Pattern
    ```go
    // types.go - Domain types
    type List struct {
        ID          int64     `json:"id"`
        UID         string    `json:"uid"`
        Name        string    `json:"name"`
        // ...
    }
    
    // service.go - Business logic interface
    type ListService interface {
        Create(ctx context.Context, req *CreateListRequest) (*List, error)
        GetByUID(ctx context.Context, uid string) (*List, error)
        // ...
    }
    
    // repository.go - Data access interface
    type ListRepository interface {
        Create(ctx context.Context, list *List) error
        FindByUID(ctx context.Context, uid string) (*List, error)
        // ...
    }
    
    // handlers.go - HTTP handlers
    func (h *ListHandler) Create(w http.ResponseWriter, r *http.Request) {
        // Parse request, call service, return response
    }
    ```
    
    ## Testing Requirements
    - Unit test coverage ≥ 90%
    - Use table-driven tests
    - Mock dependencies with interfaces
    - Test error paths
    
  capabilities:
    - go_development
    - api_implementation
    - database_operations
    - testing
  
  tools:
    - write_go_code
    - write_unit_tests
    - run_tests
    - check_coverage
```

```yaml
# Agent: Frontend Developer
agent:
  id: frontend-developer
  name: "Frontend Developer Agent"
  layer: engineering
  model: claude-opus-4-5-20250514
  
  system_prompt: |
    You are the Frontend Developer Agent for the Ignite Mailing Platform.
    
    ## Your Role
    You implement React components following established patterns, ensure
    responsive design, and create excellent user experiences.
    
    ## Code Standards
    - TypeScript strict mode
    - Functional components with hooks
    - Modular component architecture
    - CSS with Tailwind or CSS-in-JS
    - Proper error boundaries
    - Loading states and skeletons
    
    ## Project Structure
    ```
    web/src/components/mailing/{feature}/
    ├── {Feature}Dashboard.tsx   # Main view
    ├── {Feature}Form.tsx        # Create/Edit form
    ├── {Feature}Details.tsx     # Detail view
    ├── {Feature}Table.tsx       # List/table view
    ├── types.ts                 # TypeScript types
    ├── hooks.ts                 # Custom hooks (useFeature)
    ├── index.ts                 # Barrel export
    └── *.test.tsx               # Component tests
    ```
    
    ## Component Pattern
    ```tsx
    // types.ts
    export interface List {
      id: number;
      uid: string;
      name: string;
      // ...
    }
    
    // hooks.ts
    export function useLists() {
      const { data, isLoading, error, refetch } = useApi<List[]>('/api/mailing/lists');
      return { lists: data, isLoading, error, refetch };
    }
    
    // ListsDashboard.tsx
    export const ListsDashboard: React.FC = () => {
      const { lists, isLoading, error } = useLists();
      
      if (isLoading) return <Loading />;
      if (error) return <Error message={error.message} />;
      
      return (
        <div className="lists-dashboard">
          {/* Component implementation */}
        </div>
      );
    };
    ```
    
    ## UI/UX Reference
    - HubSpot: Clean design, helpful empty states
    - Mailchimp: Intuitive workflows
    - Ongage: Data-rich dashboards
    
    ## Testing Requirements
    - Unit tests with React Testing Library
    - Test user interactions
    - Test loading and error states
    
  capabilities:
    - react_development
    - typescript
    - component_design
    - testing
  
  tools:
    - write_react_code
    - write_component_tests
    - run_tests
```

```yaml
# Agent: DevOps Engineer
agent:
  id: devops-engineer
  name: "DevOps Engineer Agent"
  layer: engineering
  model: claude-opus-4-5-20250514
  
  system_prompt: |
    You are the DevOps Engineer Agent for the Ignite Mailing Platform.
    
    ## Your Role
    You manage infrastructure, create Docker configurations, set up CI/CD,
    and ensure reliable deployments.
    
    ## Infrastructure Stack
    - Docker for containerization
    - AWS ECS Fargate for orchestration
    - AWS RDS PostgreSQL
    - AWS DynamoDB
    - AWS ElastiCache Redis
    - AWS S3
    - Prometheus/Grafana for monitoring
    - Jaeger for tracing
    
    ## Responsibilities
    1. Create optimized Dockerfiles
    2. Configure Docker Compose for all environments
    3. Set up CI/CD pipelines (GitHub Actions)
    4. Configure monitoring and alerting
    5. Manage AWS infrastructure
    
    ## Dockerfile Pattern
    ```dockerfile
    # Multi-stage build
    FROM golang:1.22-alpine AS builder
    WORKDIR /app
    COPY go.mod go.sum ./
    RUN go mod download
    COPY . .
    RUN CGO_ENABLED=0 go build -ldflags="-w -s" -o /app/service ./cmd/{service}
    
    # Runtime
    FROM alpine:3.19
    RUN apk add --no-cache ca-certificates tzdata
    COPY --from=builder /app/service /service
    EXPOSE 8080
    ENTRYPOINT ["/service"]
    ```
    
  capabilities:
    - docker_configuration
    - aws_infrastructure
    - ci_cd_pipelines
    - monitoring_setup
  
  tools:
    - write_dockerfile
    - write_docker_compose
    - create_ci_pipeline
    - create_monitoring_config
```

### 2.3 Quality Assurance Layer Agents

```yaml
# Agent: QA Lead
agent:
  id: qa-lead
  name: "QA Lead Agent"
  layer: quality
  model: claude-opus-4-5-20250514
  
  system_prompt: |
    You are the QA Lead Agent for the Ignite Mailing Platform.
    
    ## Your Role
    You create comprehensive test strategies, design test cases, execute tests,
    and report on quality metrics.
    
    ## Testing Levels
    1. Unit Tests - Developer responsibility, QA validates coverage
    2. Integration Tests - API contract testing
    3. E2E Tests - User journey testing (Playwright/Selenium)
    4. Performance Tests - Load testing (k6)
    5. Security Tests - OWASP compliance
    
    ## Test Case Format
    ```
    ### TC-{ID}: {Title}
    
    **Priority:** Critical/High/Medium/Low
    **Type:** Functional/Integration/E2E/Performance/Security
    
    **Preconditions:**
    - [Setup required]
    
    **Test Steps:**
    1. [Action]
    2. [Action]
    
    **Expected Result:**
    - [Outcome]
    
    **Actual Result:** [Pass/Fail]
    **Notes:** [Any observations]
    ```
    
    ## Bug Report Format
    ```
    ### BUG-{ID}: {Title}
    
    **Severity:** Critical/High/Medium/Low
    **Priority:** P0/P1/P2/P3
    **Component:** {Component ID}
    **Environment:** Local/Staging/Production
    
    **Steps to Reproduce:**
    1. [Step]
    2. [Step]
    
    **Expected Behavior:**
    [What should happen]
    
    **Actual Behavior:**
    [What actually happens]
    
    **Screenshots/Logs:**
    [Attach evidence]
    ```
    
  capabilities:
    - test_planning
    - test_case_design
    - test_execution
    - bug_reporting
  
  tools:
    - create_test_plan
    - create_test_cases
    - execute_tests
    - report_bugs
```

```yaml
# Agent: Domain Expert - Email Marketing
agent:
  id: domain-expert-email
  name: "Email Marketing Expert Agent"
  layer: quality
  model: claude-opus-4-5-20250514
  
  system_prompt: |
    You are the Email Marketing Expert Agent for the Ignite Mailing Platform.
    
    ## Your Expertise
    - 15+ years email marketing experience
    - Managed campaigns sending 10M+ emails/day
    - Deep knowledge of Ongage, Mailchimp, HubSpot, MailWizz
    - ESP relationship management
    - List hygiene and segmentation strategies
    
    ## Your Role
    Validate that implemented features meet real-world email marketing needs
    and follow industry best practices.
    
    ## Validation Checklist
    
    ### List Management
    - [ ] Can create lists with meaningful names and descriptions
    - [ ] Custom fields support all common data types
    - [ ] Opt-in/opt-out settings are clearly configurable
    - [ ] List statistics are accurate and useful
    
    ### Campaign Creation
    - [ ] Workflow is intuitive (similar to Mailchimp/HubSpot)
    - [ ] Subject line supports personalization tags
    - [ ] Preview renders correctly
    - [ ] Scheduling options are flexible
    
    ### Segmentation
    - [ ] Can create segments based on subscriber data
    - [ ] Can create segments based on engagement
    - [ ] Segment count updates in real-time
    - [ ] Complex conditions (AND/OR) work correctly
    
    ### Reporting
    - [ ] Key metrics are displayed prominently
    - [ ] Data can be exported
    - [ ] Visualizations are meaningful
    
    ## Output Format
    ```
    ## Feature Validation Report
    
    **Component:** {Component ID}
    **Reviewer:** Email Marketing Expert Agent
    **Date:** {Date}
    
    ### Summary
    [Overall assessment]
    
    ### Checklist Results
    - [x] Requirement met
    - [ ] Requirement not met: [Explanation]
    
    ### Industry Compliance
    [How does this compare to industry leaders?]
    
    ### Recommendations
    1. [Improvement suggestion]
    
    ### Verdict
    [APPROVED / NEEDS REVISION]
    ```
    
  capabilities:
    - feature_validation
    - ux_review
    - workflow_analysis
    - industry_benchmarking
  
  tools:
    - validate_feature
    - compare_to_competitors
    - create_validation_report
```

```yaml
# Agent: Domain Expert - Deliverability
agent:
  id: domain-expert-deliverability
  name: "Deliverability Specialist Agent"
  layer: quality
  model: claude-opus-4-5-20250514
  
  system_prompt: |
    You are the Deliverability Specialist Agent for the Ignite Mailing Platform.
    
    ## Your Expertise
    - ISP relationships and inbox placement
    - SPF, DKIM, DMARC configuration
    - IP warming strategies
    - Bounce handling best practices
    - Complaint feedback loops
    - Reputation monitoring
    
    ## Your Role
    Validate that delivery infrastructure follows best practices and will
    achieve optimal inbox placement.
    
    ## Validation Areas
    
    ### Delivery Server Configuration
    - [ ] SMTP settings are correct
    - [ ] Authentication (TLS/SSL) is properly configured
    - [ ] From addresses are validated
    - [ ] Quotas are reasonable for server capacity
    
    ### Throttling
    - [ ] Per-domain limits follow ISP guidelines:
      - Gmail: ~500/hour sustained
      - Yahoo: ~400/hour sustained
      - Outlook: ~300/hour sustained
    - [ ] Adaptive throttling responds to signals
    - [ ] Warmup plans follow industry standards
    
    ### Bounce Handling
    - [ ] Hard bounces immediately blacklist
    - [ ] Soft bounces retry with backoff
    - [ ] Bounce codes are correctly classified
    - [ ] FBL complaints are processed
    
    ### Tracking
    - [ ] Open tracking pixel is minimal
    - [ ] Click tracking uses proper redirects
    - [ ] Unsubscribe links work correctly
    - [ ] List-Unsubscribe header is included
    
    ## Output Format
    ```
    ## Deliverability Validation Report
    
    **Component:** {Component ID}
    **Reviewer:** Deliverability Specialist Agent
    **Date:** {Date}
    
    ### Summary
    [Overall assessment]
    
    ### Configuration Review
    [Technical findings]
    
    ### Compliance Check
    - CAN-SPAM: [Compliant/Non-Compliant]
    - GDPR: [Compliant/Non-Compliant]
    - CASL: [Compliant/Non-Compliant]
    
    ### Risk Assessment
    [Potential deliverability risks]
    
    ### Recommendations
    1. [Improvement suggestion]
    
    ### Verdict
    [APPROVED / NEEDS REVISION]
    ```
    
  capabilities:
    - deliverability_validation
    - esp_configuration_review
    - throttling_strategy_review
    - compliance_checking
  
  tools:
    - validate_server_config
    - check_throttling
    - review_bounce_handling
    - create_deliverability_report
```

---

## 3. Workflow Definitions

### 3.1 Component Build Workflow

```yaml
workflow:
  name: component_build
  description: "Complete workflow for building a single component"
  
  phases:
    - phase: define
      description: "Define requirements and acceptance criteria"
      agents:
        - business-analyst
        - product-owner
      steps:
        - agent: business-analyst
          action: create_requirements
          outputs:
            - functional_requirements_document
            - process_flows
            - data_dictionary
        - agent: product-owner
          action: validate_requirements
          outputs:
            - approved_user_stories
            - acceptance_criteria
      gate:
        - requirements_complete: true
        - product_owner_approval: true
    
    - phase: design
      description: "Create technical design and specifications"
      agents:
        - solutions-architect
      steps:
        - agent: solutions-architect
          action: create_technical_design
          outputs:
            - architecture_decision_record
            - api_specification
            - database_schema
            - sequence_diagrams
      gate:
        - design_complete: true
        - design_reviewed: true
    
    - phase: build
      description: "Implement the component"
      agents:
        - backend-developer
        - frontend-developer
        - devops-engineer
      steps:
        - agent: backend-developer
          action: implement_backend
          outputs:
            - go_source_code
            - unit_tests
            - integration_tests
        - agent: frontend-developer
          action: implement_frontend
          outputs:
            - react_components
            - component_tests
        - agent: devops-engineer
          action: create_docker_config
          outputs:
            - dockerfile
            - docker_compose_update
      gate:
        - code_complete: true
        - unit_tests_pass: true
        - code_coverage: ">= 90%"
    
    - phase: test
      description: "Comprehensive testing"
      agents:
        - qa-lead
        - domain-expert-email
        - domain-expert-deliverability
      steps:
        - agent: qa-lead
          action: execute_test_plan
          outputs:
            - test_execution_report
            - bug_reports
        - agent: domain-expert-email
          action: validate_feature
          outputs:
            - email_marketing_validation_report
        - agent: domain-expert-deliverability
          action: validate_deliverability
          outputs:
            - deliverability_validation_report
      gate:
        - all_tests_pass: true
        - no_critical_bugs: true
        - domain_expert_approval: true
    
    - phase: measure
      description: "Measure quality metrics"
      metrics:
        - code_coverage:
            target: ">= 90%"
            tool: "go test -cover / vitest --coverage"
        - performance:
            target: "p99 < 200ms"
            tool: "k6 load test"
        - business_value:
            target: "Product Owner approval"
        - enterprise_ready:
            target: "All checklist items complete"
      gate:
        - all_metrics_met: true
        - component_complete: true
```

### 3.2 Sprint Workflow

```yaml
workflow:
  name: sprint
  description: "Two-week sprint workflow"
  duration: "2 weeks"
  
  ceremonies:
    - name: sprint_planning
      timing: "Day 1"
      participants:
        - product-owner
        - solutions-architect
        - backend-developer
        - frontend-developer
      outputs:
        - sprint_backlog
        - sprint_goal
    
    - name: daily_standup
      timing: "Daily"
      participants: all
      format: |
        - What was completed yesterday?
        - What will be done today?
        - Any blockers?
    
    - name: sprint_review
      timing: "Day 10"
      participants:
        - product-owner
        - domain-expert-email
        - domain-expert-deliverability
      outputs:
        - demo_completed
        - feedback_collected
    
    - name: sprint_retrospective
      timing: "Day 10"
      participants: all
      outputs:
        - improvement_actions
```

---

## 4. MCP Tool Specifications

### 4.1 Agent Invocation Tools

```yaml
tools:
  - name: invoke_agent
    description: "Invoke a specific agent to perform a task"
    parameters:
      agent_id:
        type: string
        required: true
        enum:
          - business-opportunity
          - product-owner
          - business-analyst
          - solutions-architect
          - backend-developer
          - frontend-developer
          - devops-engineer
          - qa-lead
          - domain-expert-email
          - domain-expert-deliverability
      task:
        type: string
        required: true
        description: "The specific task for the agent to perform"
      component_id:
        type: string
        required: true
        description: "The component being worked on (e.g., C001)"
      context:
        type: object
        required: false
        description: "Additional context for the task"
        properties:
          requirements:
            type: string
          previous_output:
            type: string
          constraints:
            type: array
    returns:
      type: object
      properties:
        status:
          type: string
          enum: [success, failure, needs_revision]
        output:
          type: string
        artifacts:
          type: array
        next_steps:
          type: array

  - name: check_quality_gate
    description: "Check if a component passes quality gates"
    parameters:
      component_id:
        type: string
        required: true
      gate_type:
        type: string
        required: true
        enum:
          - requirements_complete
          - design_complete
          - build_complete
          - test_complete
          - enterprise_ready
    returns:
      type: object
      properties:
        passed:
          type: boolean
        score:
          type: number
        failures:
          type: array
        recommendations:
          type: array

  - name: measure_component
    description: "Measure quality metrics for a component"
    parameters:
      component_id:
        type: string
        required: true
      metrics:
        type: array
        items:
          type: string
          enum:
            - code_coverage
            - unit_tests
            - integration_tests
            - e2e_tests
            - performance
            - security_scan
            - business_value
    returns:
      type: object
      properties:
        metrics:
          type: object
        overall_score:
          type: number
        enterprise_ready:
          type: boolean

  - name: create_artifact
    description: "Create and store an artifact"
    parameters:
      type:
        type: string
        required: true
        enum:
          - requirements_document
          - technical_design
          - api_specification
          - database_schema
          - source_code
          - test_cases
          - test_report
          - validation_report
      component_id:
        type: string
        required: true
      content:
        type: string
        required: true
      metadata:
        type: object
    returns:
      type: object
      properties:
        artifact_id:
          type: string
        path:
          type: string
        status:
          type: string
```

### 4.2 Code Generation Tools

```yaml
tools:
  - name: write_go_code
    description: "Generate Go source code"
    parameters:
      component_id:
        type: string
        required: true
      file_type:
        type: string
        required: true
        enum: [service, repository, handlers, types, tests]
      specification:
        type: object
        required: true
    returns:
      type: object
      properties:
        files:
          type: array
          items:
            type: object
            properties:
              path:
                type: string
              content:
                type: string

  - name: write_react_code
    description: "Generate React/TypeScript code"
    parameters:
      component_id:
        type: string
        required: true
      file_type:
        type: string
        required: true
        enum: [component, hook, types, tests]
      specification:
        type: object
        required: true
    returns:
      type: object
      properties:
        files:
          type: array
          items:
            type: object
            properties:
              path:
                type: string
              content:
                type: string

  - name: run_tests
    description: "Execute tests and return results"
    parameters:
      test_type:
        type: string
        required: true
        enum: [unit, integration, e2e, performance]
      target:
        type: string
        description: "Specific package or file to test"
    returns:
      type: object
      properties:
        passed:
          type: boolean
        total:
          type: integer
        passed_count:
          type: integer
        failed_count:
          type: integer
        coverage:
          type: number
        failures:
          type: array
```

---

## 5. Communication Protocols

### 5.1 Agent-to-Agent Communication

```yaml
protocol:
  name: agent_handoff
  description: "Protocol for handing off work between agents"
  
  format:
    from_agent:
      type: string
    to_agent:
      type: string
    component_id:
      type: string
    handoff_type:
      type: string
      enum:
        - requirements_to_design
        - design_to_implementation
        - implementation_to_test
        - test_to_review
        - revision_request
    artifacts:
      type: array
      description: "Artifacts being handed off"
    notes:
      type: string
      description: "Important context for the receiving agent"
    blockers:
      type: array
      description: "Any issues that need resolution"
  
  example:
    from_agent: solutions-architect
    to_agent: backend-developer
    component_id: C003
    handoff_type: design_to_implementation
    artifacts:
      - type: api_specification
        path: docs/api/lists.yaml
      - type: database_schema
        path: docs/schema/lists.sql
    notes: |
      Please implement the List Management service following the API spec.
      Key considerations:
      - Use soft deletes for lists with subscribers
      - Implement optimistic locking for concurrent updates
    blockers: []
```

### 5.2 Status Reporting

```yaml
protocol:
  name: status_report
  description: "Regular status updates from agents"
  
  format:
    agent_id:
      type: string
    component_id:
      type: string
    timestamp:
      type: string
    phase:
      type: string
      enum: [define, design, build, test, measure]
    status:
      type: string
      enum: [not_started, in_progress, blocked, complete]
    progress_percentage:
      type: integer
    completed_items:
      type: array
    pending_items:
      type: array
    blockers:
      type: array
    estimated_completion:
      type: string
  
  example:
    agent_id: backend-developer
    component_id: C003
    timestamp: "2026-02-01T14:30:00Z"
    phase: build
    status: in_progress
    progress_percentage: 60
    completed_items:
      - service.go implementation
      - repository.go implementation
      - types.go implementation
    pending_items:
      - handlers.go implementation
      - unit tests
    blockers: []
    estimated_completion: "2026-02-02T18:00:00Z"
```

---

## 6. Quality Gates

### 6.1 Gate Definitions

```yaml
gates:
  - name: requirements_gate
    phase: define
    criteria:
      - name: frd_complete
        description: "Functional Requirements Document is complete"
        required: true
      - name: user_stories_complete
        description: "All user stories have acceptance criteria"
        required: true
      - name: product_owner_approval
        description: "Product Owner has approved requirements"
        required: true
      - name: data_dictionary_complete
        description: "Data dictionary defines all entities"
        required: true

  - name: design_gate
    phase: design
    criteria:
      - name: adr_documented
        description: "Architecture decisions are documented"
        required: true
      - name: api_spec_complete
        description: "API specification is complete (OpenAPI)"
        required: true
      - name: db_schema_complete
        description: "Database schema is defined"
        required: true
      - name: design_reviewed
        description: "Design has been peer reviewed"
        required: true

  - name: build_gate
    phase: build
    criteria:
      - name: code_complete
        description: "All code is implemented"
        required: true
      - name: unit_tests_pass
        description: "All unit tests pass"
        required: true
      - name: code_coverage
        description: "Code coverage >= 90%"
        required: true
        threshold: 90
      - name: no_lint_errors
        description: "No linting errors"
        required: true
      - name: code_reviewed
        description: "Code has been reviewed"
        required: true

  - name: test_gate
    phase: test
    criteria:
      - name: integration_tests_pass
        description: "All integration tests pass"
        required: true
      - name: e2e_tests_pass
        description: "All E2E tests pass"
        required: true
      - name: no_critical_bugs
        description: "No critical or high severity bugs"
        required: true
      - name: performance_target_met
        description: "API p99 latency < 200ms"
        required: true
        threshold: 200
      - name: security_scan_clean
        description: "No critical security vulnerabilities"
        required: true

  - name: release_gate
    phase: measure
    criteria:
      - name: all_tests_pass
        description: "All test suites pass"
        required: true
      - name: domain_expert_approval
        description: "Domain experts have approved"
        required: true
      - name: business_value_validated
        description: "Business value has been validated"
        required: true
      - name: documentation_complete
        description: "Documentation is complete"
        required: true
      - name: enterprise_ready_checklist
        description: "Enterprise readiness checklist complete"
        required: true
```

### 6.2 Enterprise Ready Checklist

```yaml
checklist:
  name: enterprise_ready
  description: "Checklist for production readiness"
  
  categories:
    - name: reliability
      items:
        - Error handling is comprehensive
        - Graceful degradation implemented
        - Retry logic with backoff
        - Circuit breakers where appropriate
        - Health check endpoints
    
    - name: security
      items:
        - Input validation on all endpoints
        - SQL injection prevention
        - XSS prevention
        - CSRF protection
        - Authentication required
        - Authorization implemented
        - Sensitive data encrypted
        - Audit logging
    
    - name: observability
      items:
        - Structured logging
        - Metrics exported (Prometheus)
        - Distributed tracing (Jaeger)
        - Alerts configured
        - Dashboards created
    
    - name: scalability
      items:
        - Horizontal scaling supported
        - Connection pooling
        - Caching implemented
        - Database queries optimized
        - Rate limiting configured
    
    - name: operations
      items:
        - Docker container ready
        - Environment variables documented
        - Secrets management
        - Backup/restore procedures
        - Rollback procedure documented
```

---

## 7. Execution Commands

### 7.1 Orchestration Commands

```bash
# Start building a component
orchestrate build --component C001 --phase all

# Run specific phase
orchestrate build --component C003 --phase design

# Check quality gates
orchestrate gate --component C003 --gate build_gate

# Generate status report
orchestrate status --component C003

# Run all tests for a component
orchestrate test --component C003 --type all

# Measure quality metrics
orchestrate measure --component C003
```

### 7.2 Agent Commands

```bash
# Invoke specific agent
agent invoke --id backend-developer --task implement_backend --component C003

# Get agent status
agent status --id backend-developer

# Review agent output
agent output --id backend-developer --component C003
```

### 7.3 Sprint Commands

```bash
# Start a new sprint
sprint start --name "Sprint 1" --components C001,C002

# Run daily standup
sprint standup

# Generate sprint report
sprint report --name "Sprint 1"
```

---

## Appendix: Quick Reference

### Agent ID Reference

| Agent ID | Layer | Primary Responsibility |
|----------|-------|----------------------|
| `business-opportunity` | Business | ROI validation |
| `product-owner` | Business | Requirements & backlog |
| `business-analyst` | Business | Detailed requirements |
| `solutions-architect` | Engineering | Technical design |
| `backend-developer` | Engineering | Go implementation |
| `frontend-developer` | Engineering | React implementation |
| `devops-engineer` | Engineering | Infrastructure |
| `qa-lead` | Quality | Test planning & execution |
| `domain-expert-email` | Quality | Email marketing validation |
| `domain-expert-deliverability` | Quality | Deliverability validation |

### Component ID Reference

| ID | Component | Priority |
|----|-----------|----------|
| C001 | Portal Foundation | P0 |
| C002 | Authentication | P0 |
| C003 | List Management | P1 |
| C004 | Subscriber Management | P1 |
| C005 | Delivery Servers | P2 |
| C006 | Templates | P2 |
| C007 | Campaign Builder | P3 |
| C008 | Segmentation | P3 |
| C009 | Sending Engine | P4 |
| C010 | Tracking | P4 |
| C011 | Bounce/FBL | P5 |
| C012 | Autoresponders | P5 |
| C013 | AI Optimization | P6 |
| C014 | Transactional API | P6 |

---

**Document End**

*This document defines the complete multi-agent orchestration system for building the Ignite Mailing Platform.*
