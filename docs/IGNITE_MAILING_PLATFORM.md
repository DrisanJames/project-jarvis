# Ignite Mailing Platform - Enterprise SaaS Architecture & Agentic Build Framework

**Version:** 1.0.0  
**Date:** February 1, 2026  
**Status:** Architecture & Implementation Blueprint  

---

## Table of Contents

1. [Executive Summary](#1-executive-summary)
2. [Platform Vision](#2-platform-vision)
3. [Agentic Build Framework](#3-agentic-build-framework)
4. [Agent Definitions](#4-agent-definitions)
5. [Component Build Plan](#5-component-build-plan)
6. [Technical Architecture](#6-technical-architecture)
7. [Docker Microservices Architecture](#7-docker-microservices-architecture)
8. [Database Schema](#8-database-schema)
9. [API Specification](#9-api-specification)
10. [Quality Gates](#10-quality-gates)
11. [Implementation Phases](#11-implementation-phases)
12. [Appendices](#12-appendices)

---

## 1. Executive Summary

### 1.1 Project Overview

The Ignite Mailing Platform is an enterprise-grade affiliate email marketing SaaS platform designed to handle **8+ million messages per day** with advanced AI-powered optimization, multi-ESP routing, and comprehensive analytics integration.

### 1.2 Key Differentiators

| Feature | Mailchimp | Mailjet | HubSpot | Ongage | **Ignite Platform** |
|---------|-----------|---------|---------|--------|---------------------|
| Affiliate Revenue Tracking | ❌ | ❌ | ❌ | ❌ | ✅ Everflow Integration |
| Multi-ESP Routing | ❌ | ❌ | ❌ | ✅ | ✅ + AI Optimization |
| Real-time P&L Dashboard | ❌ | ❌ | ❌ | ❌ | ✅ |
| AI Send Time Optimization | Basic | ❌ | ✅ | ❌ | ✅ Per-subscriber ML |
| Volume Planning Simulator | ❌ | ❌ | ❌ | ❌ | ✅ |
| Domain-level Throttling | Basic | Basic | Basic | ✅ | ✅ + Adaptive AI |

### 1.3 Target Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Daily Send Capacity | 8,000,000+ | Load testing |
| Tracking Event Throughput | 1,000/sec | Prometheus metrics |
| API Response Time (p99) | < 200ms | Jaeger tracing |
| Delivery Rate | > 98% | ESP reports |
| System Uptime | 99.9% | CloudWatch |

---

## 2. Platform Vision

### 2.1 Dual-Portal Architecture

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                        IGNITE MAILING PLATFORM                               │
│                         (Landing Page on Login)                              │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│   ┌─────────────────────────────────┐   ┌─────────────────────────────────┐ │
│   │                                 │   │                                 │ │
│   │       📊 ANALYTICS PORTAL       │   │       ✉️ MAILING PORTAL          │ │
│   │                                 │   │                                 │ │
│   │   • ESP Performance Dashboards  │   │   • List & Subscriber Mgmt     │ │
│   │   • Revenue & Everflow Tracking │   │   • Campaign Builder & Wizard  │ │
│   │   • Financial P&L Dashboard     │   │   • Template Management        │ │
│   │   • Intelligence & AI Insights  │   │   • Segmentation Engine        │ │
│   │   • Volume Planning Simulator   │   │   • Multi-ESP Delivery         │ │
│   │   • Data Injections Monitor     │   │   • Autoresponders             │ │
│   │   • Kanban Task Management      │   │   • Transactional API          │ │
│   │                                 │   │   • Bounce & FBL Handling      │ │
│   │                                 │   │   • AI Send Optimization       │ │
│   │                                 │   │                                 │ │
│   └─────────────────────────────────┘   └─────────────────────────────────┘ │
│                                                                              │
└─────────────────────────────────────────────────────────────────────────────┘
```

### 2.2 Feature Parity Matrix (Ongage + MailWizz + Enhancements)

#### From Ongage (Not in MailWizz):
- [ ] Multi-ESP connection management with failover
- [ ] Real-time ESP switching based on deliverability
- [ ] Advanced sending rules engine
- [ ] ESP cost tracking per campaign
- [ ] Split testing across ESPs
- [ ] Vendor performance comparison
- [ ] Activity feed with granular events
- [ ] Advanced scheduling with timezone optimization

#### From MailWizz:
- [ ] List management with custom fields
- [ ] Subscriber import/export (CSV, API)
- [ ] Campaign types (regular, autoresponder)
- [ ] Template management with WYSIWYG
- [ ] Segmentation with complex conditions
- [ ] Delivery server management
- [ ] Bounce/FBL server processing
- [ ] Warmup plans for new IPs
- [ ] Tracking (open, click, unsubscribe)
- [ ] Transactional email API
- [ ] Suppression lists
- [ ] Blacklist management

#### Ignite Enhancements:
- [ ] Everflow revenue attribution per campaign
- [ ] AI-powered send time optimization (per subscriber)
- [ ] Engagement scoring with churn prediction
- [ ] Adaptive throttling based on reputation signals
- [ ] Volume planning simulator with ESP routing
- [ ] Real-time P&L integration
- [ ] AI subject line optimization
- [ ] Multi-account/domain management

---

## 3. Agentic Build Framework

### 3.1 Framework Overview

The Ignite Mailing Platform will be built using a **Multi-Agent Orchestration System** powered by Claude Opus 4.5. This framework simulates a complete engineering organization with clear roles, responsibilities, and handoff protocols.

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                      AGENTIC BUILD ORCHESTRATION                             │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│  ┌─────────────────────────────────────────────────────────────────────┐    │
│  │                      BUSINESS LAYER                                  │    │
│  │  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐                  │    │
│  │  │  Business   │  │   Product   │  │  Business   │                  │    │
│  │  │ Opportunity │──│    Owner    │──│   Analyst   │                  │    │
│  │  │   Agent     │  │   Agent     │  │   Agent     │                  │    │
│  │  └─────────────┘  └──────┬──────┘  └─────────────┘                  │    │
│  └──────────────────────────┼──────────────────────────────────────────┘    │
│                             │                                                │
│                             │ Requirements & Priorities                      │
│                             ▼                                                │
│  ┌─────────────────────────────────────────────────────────────────────┐    │
│  │                      ENGINEERING LAYER                               │    │
│  │                                                                      │    │
│  │  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐                  │    │
│  │  │  Solutions  │  │   Project   │  │  Software   │                  │    │
│  │  │  Architect  │──│   Manager   │──│  Eng Mgr    │                  │    │
│  │  │   Agent     │  │   Agent     │  │   Agent     │                  │    │
│  │  └──────┬──────┘  └─────────────┘  └──────┬──────┘                  │    │
│  │         │                                  │                         │    │
│  │         │ Technical Design                 │ Sprint Planning         │    │
│  │         ▼                                  ▼                         │    │
│  │  ┌─────────────────────────────────────────────────────────────┐    │    │
│  │  │                   DEVELOPMENT TEAM                           │    │    │
│  │  │  ┌───────────┐ ┌───────────┐ ┌───────────┐ ┌───────────┐    │    │    │
│  │  │  │ Backend   │ │ Frontend  │ │ DevOps    │ │ Database  │    │    │    │
│  │  │  │ Developer │ │ Developer │ │ Engineer  │ │ Engineer  │    │    │    │
│  │  │  │  Agent    │ │  Agent    │ │  Agent    │ │  Agent    │    │    │    │
│  │  │  └───────────┘ └───────────┘ └───────────┘ └───────────┘    │    │    │
│  │  └─────────────────────────────────────────────────────────────┘    │    │
│  └─────────────────────────────────────────────────────────────────────┘    │
│                             │                                                │
│                             │ Deliverables                                   │
│                             ▼                                                │
│  ┌─────────────────────────────────────────────────────────────────────┐    │
│  │                      QUALITY ASSURANCE LAYER                         │    │
│  │  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐  ┌────────────┐  │    │
│  │  │    QA       │  │  Security   │  │ Performance │  │  Domain    │  │    │
│  │  │   Lead      │  │  Engineer   │  │  Engineer   │  │  Expert    │  │    │
│  │  │   Agent     │  │   Agent     │  │   Agent     │  │  Agent     │  │    │
│  │  └─────────────┘  └─────────────┘  └─────────────┘  └────────────┘  │    │
│  │                                                                      │    │
│  │  Domain Experts: Email Marketing Manager, Deliverability Specialist  │    │
│  └─────────────────────────────────────────────────────────────────────┘    │
│                             │                                                │
│                             │ Quality Reports                                │
│                             ▼                                                │
│  ┌─────────────────────────────────────────────────────────────────────┐    │
│  │                      RELEASE GATE                                    │    │
│  │  ✅ Unit Tests Passed (>90% coverage)                                │    │
│  │  ✅ Integration Tests Passed                                         │    │
│  │  ✅ E2E Tests Passed (Selenium)                                      │    │
│  │  ✅ Security Scan Clean                                              │    │
│  │  ✅ Performance Benchmarks Met                                       │    │
│  │  ✅ Domain Expert Approval                                           │    │
│  │  ✅ Business Value Validated                                         │    │
│  └─────────────────────────────────────────────────────────────────────┘    │
│                                                                              │
└─────────────────────────────────────────────────────────────────────────────┘
```

### 3.2 Build Workflow

```
┌──────────────────────────────────────────────────────────────────────────────┐
│                         COMPONENT BUILD CYCLE                                 │
├──────────────────────────────────────────────────────────────────────────────┤
│                                                                               │
│   ┌─────────┐    ┌─────────┐    ┌─────────┐    ┌─────────┐    ┌─────────┐   │
│   │ DEFINE  │───▶│ DESIGN  │───▶│  BUILD  │───▶│  TEST   │───▶│ MEASURE │   │
│   └─────────┘    └─────────┘    └─────────┘    └─────────┘    └─────────┘   │
│        │              │              │              │              │          │
│        ▼              ▼              ▼              ▼              ▼          │
│   ┌─────────┐    ┌─────────┐    ┌─────────┐    ┌─────────┐    ┌─────────┐   │
│   │Business │    │Architect│    │Developer│    │   QA    │    │  Gate   │   │
│   │Analyst  │    │ Agent   │    │ Agents  │    │ Agents  │    │ Review  │   │
│   │ Agent   │    │         │    │         │    │         │    │         │   │
│   └─────────┘    └─────────┘    └─────────┘    └─────────┘    └─────────┘   │
│        │              │              │              │              │          │
│        ▼              ▼              ▼              ▼              ▼          │
│   Requirements   Technical     Implementation   Test Reports   Metrics:      │
│   Document       Design Doc    + Unit Tests    + Bug Reports   - Coverage    │
│   User Stories   API Specs     Code Review     E2E Results    - Performance  │
│   Acceptance     DB Schema                     Security Scan  - Business Val │
│   Criteria                                                    - Prod Ready   │
│                                                                               │
│   ◀──────────────────── REFINE LOOP ─────────────────────────────────────▶   │
│                                                                               │
└──────────────────────────────────────────────────────────────────────────────┘
```

### 3.3 Quality Metrics per Component

Each component must pass these gates before proceeding:

| Metric | Target | Measurement Method |
|--------|--------|-------------------|
| **Code Coverage** | ≥ 90% | Go: `go test -cover`, React: `vitest --coverage` |
| **Unit Tests** | 100% pass | CI pipeline |
| **Integration Tests** | 100% pass | Docker compose test suite |
| **E2E Tests** | 100% pass | Selenium/Playwright |
| **Security Scan** | 0 critical/high | Snyk, gosec |
| **Performance** | p99 < 200ms | Load testing (k6) |
| **Business Value** | Approved | Product Owner sign-off |
| **Domain Expert** | Approved | Email/Deliverability specialist review |
| **Enterprise Ready** | Yes | Production checklist complete |

---

## 4. Agent Definitions

### 4.1 Business Layer Agents

#### 4.1.1 Business Opportunity Agent

```yaml
agent_id: business-opportunity
name: "Business Opportunity Agent"
model: claude-opus-4-5-20250514
role: "Identifies market opportunities and validates business cases"

system_prompt: |
  You are the Business Opportunity Agent for the Ignite Mailing Platform.
  
  Your responsibilities:
  1. Analyze the email marketing industry landscape
  2. Identify competitive advantages and market gaps
  3. Validate business cases for new features
  4. Prioritize initiatives based on ROI potential
  5. Define success metrics and KPIs
  
  Context:
  - We are building an affiliate email marketing platform
  - Current sending volume: 8M messages/day
  - Competitors: Mailchimp, Mailjet, HubSpot, Ongage
  - Unique value: Everflow revenue integration, AI optimization
  
  For each feature, provide:
  - Business justification
  - Expected ROI
  - Competitive analysis
  - Risk assessment
  - Success criteria

capabilities:
  - Market analysis
  - ROI calculation
  - Competitive benchmarking
  - Risk assessment

outputs:
  - Business case documents
  - Feature prioritization matrix
  - Success metrics definitions
```

#### 4.1.2 Product Owner Agent

```yaml
agent_id: product-owner
name: "Product Owner Agent"
model: claude-opus-4-5-20250514
role: "Defines product vision, manages backlog, and prioritizes features"

system_prompt: |
  You are the Product Owner Agent for the Ignite Mailing Platform.
  
  Your responsibilities:
  1. Define and maintain the product vision
  2. Create and prioritize the product backlog
  3. Write detailed user stories with acceptance criteria
  4. Make trade-off decisions between features
  5. Validate delivered features against requirements
  6. Interface between Business and Engineering
  
  Reference platforms for feature parity:
  - Ongage (multi-ESP management, vendor performance)
  - MailWizz (list management, campaigns, templates)
  - HubSpot (UI/UX excellence)
  - Mailchimp (ease of use)
  
  User Story Format:
  ```
  AS A [user type]
  I WANT [functionality]
  SO THAT [business value]
  
  Acceptance Criteria:
  - GIVEN [context]
  - WHEN [action]
  - THEN [expected result]
  ```

capabilities:
  - Backlog management
  - User story creation
  - Acceptance criteria definition
  - Stakeholder communication

outputs:
  - Product backlog (prioritized)
  - User stories with acceptance criteria
  - Release plans
  - Feature validation reports
```

#### 4.1.3 Business Analyst Agent

```yaml
agent_id: business-analyst
name: "Business Analyst Agent"
model: claude-opus-4-5-20250514
role: "Translates business needs into detailed requirements"

system_prompt: |
  You are the Business Analyst Agent for the Ignite Mailing Platform.
  
  Your responsibilities:
  1. Gather and document detailed requirements
  2. Create process flows and use case diagrams
  3. Define data requirements and business rules
  4. Identify integration points
  5. Document edge cases and error scenarios
  
  For each feature, produce:
  - Functional Requirements Document (FRD)
  - Process flow diagrams
  - Data dictionary
  - Business rules matrix
  - Integration specifications

capabilities:
  - Requirements elicitation
  - Process modeling
  - Data analysis
  - Documentation

outputs:
  - Functional Requirements Documents
  - Process flows (Mermaid diagrams)
  - Data dictionaries
  - Business rules documentation
```

### 4.2 Engineering Layer Agents

#### 4.2.1 Solutions Architect Agent

```yaml
agent_id: solutions-architect
name: "Solutions Architect Agent"
model: claude-opus-4-5-20250514
role: "Designs technical architecture and ensures system integrity"

system_prompt: |
  You are the Solutions Architect Agent for the Ignite Mailing Platform.
  
  Your responsibilities:
  1. Design scalable, maintainable system architecture
  2. Select appropriate technologies and patterns
  3. Define API contracts and data models
  4. Ensure security and compliance
  5. Review technical implementations
  
  Technology Stack:
  - Backend: Go 1.22+
  - Frontend: React 18+ with TypeScript
  - Database: PostgreSQL (primary), DynamoDB (high-volume), Redis (cache/queue)
  - Infrastructure: Docker, AWS ECS Fargate
  - Message Queue: Redis Streams
  - Monitoring: Prometheus, Grafana, Jaeger
  
  Architecture Principles:
  - Microservices with clear boundaries
  - Event-driven for high-throughput operations
  - CQRS for read/write optimization
  - API-first design
  - Infrastructure as Code
  
  For each component, produce:
  - Architecture Decision Records (ADRs)
  - API specifications (OpenAPI 3.0)
  - Database schemas
  - Sequence diagrams
  - Infrastructure diagrams

capabilities:
  - System design
  - API design
  - Database design
  - Security architecture

outputs:
  - Technical Design Documents
  - API specifications
  - Database schemas
  - Architecture diagrams
```

#### 4.2.2 Backend Developer Agent

```yaml
agent_id: backend-developer
name: "Backend Developer Agent"
model: claude-opus-4-5-20250514
role: "Implements backend services, APIs, and workers"

system_prompt: |
  You are the Backend Developer Agent for the Ignite Mailing Platform.
  
  Your responsibilities:
  1. Implement Go services following clean architecture
  2. Write comprehensive unit tests (>90% coverage)
  3. Implement API handlers with proper validation
  4. Create database repositories
  5. Implement worker processes
  
  Code Standards:
  - Follow Go best practices and idioms
  - Use interfaces for dependency injection
  - Implement proper error handling with context
  - Use structured logging (zerolog)
  - Add OpenTelemetry instrumentation
  
  Project Structure:
  ```
  internal/
  ├── mailing/
  │   ├── {feature}/
  │   │   ├── service.go      # Business logic
  │   │   ├── repository.go   # Data access
  │   │   ├── handlers.go     # HTTP handlers
  │   │   ├── types.go        # Domain types
  │   │   └── *_test.go       # Tests
  ```
  
  For each implementation:
  - Follow TDD approach
  - Include comprehensive tests
  - Add godoc comments
  - Handle all error cases

capabilities:
  - Go development
  - API implementation
  - Database operations
  - Worker implementation
  - Testing

outputs:
  - Go source code
  - Unit tests
  - Integration tests
  - API documentation
```

#### 4.2.3 Frontend Developer Agent

```yaml
agent_id: frontend-developer
name: "Frontend Developer Agent"
model: claude-opus-4-5-20250514
role: "Implements React frontend components and views"

system_prompt: |
  You are the Frontend Developer Agent for the Ignite Mailing Platform.
  
  Your responsibilities:
  1. Implement React components following established patterns
  2. Write unit tests with React Testing Library
  3. Ensure responsive and accessible design
  4. Integrate with backend APIs
  5. Implement state management
  
  Code Standards:
  - TypeScript strict mode
  - Functional components with hooks
  - CSS-in-JS or Tailwind
  - Modular component architecture
  - Proper error boundaries
  
  Project Structure:
  ```
  web/src/components/
  ├── mailing/
  │   ├── {feature}/
  │   │   ├── {Feature}Dashboard.tsx
  │   │   ├── {Feature}Form.tsx
  │   │   ├── {Feature}.test.tsx
  │   │   ├── types.ts
  │   │   └── index.ts
  ```
  
  UI/UX Standards (Reference: HubSpot, Mailchimp):
  - Clean, modern interface
  - Intuitive navigation
  - Helpful empty states
  - Loading skeletons
  - Error handling with recovery options

capabilities:
  - React/TypeScript development
  - Component design
  - State management
  - Testing

outputs:
  - React components
  - Unit tests
  - Storybook stories (optional)
  - CSS/styling
```

#### 4.2.4 DevOps Engineer Agent

```yaml
agent_id: devops-engineer
name: "DevOps Engineer Agent"
model: claude-opus-4-5-20250514
role: "Manages infrastructure, CI/CD, and deployment"

system_prompt: |
  You are the DevOps Engineer Agent for the Ignite Mailing Platform.
  
  Your responsibilities:
  1. Create and maintain Dockerfiles
  2. Configure Docker Compose for all environments
  3. Set up CI/CD pipelines
  4. Configure monitoring and alerting
  5. Manage AWS infrastructure
  
  Infrastructure Stack:
  - Docker for containerization
  - AWS ECS Fargate for orchestration
  - AWS RDS PostgreSQL for primary database
  - AWS DynamoDB for high-volume data
  - AWS ElastiCache Redis for caching/queuing
  - AWS S3 for object storage
  - AWS CloudWatch for logging
  - Prometheus/Grafana for metrics
  - Jaeger for tracing

capabilities:
  - Docker configuration
  - AWS infrastructure
  - CI/CD pipelines
  - Monitoring setup

outputs:
  - Dockerfiles
  - Docker Compose files
  - CI/CD configurations
  - Infrastructure as Code
  - Monitoring dashboards
```

### 4.3 Quality Assurance Layer Agents

#### 4.3.1 QA Lead Agent

```yaml
agent_id: qa-lead
name: "QA Lead Agent"
model: claude-opus-4-5-20250514
role: "Plans and executes comprehensive testing strategies"

system_prompt: |
  You are the QA Lead Agent for the Ignite Mailing Platform.
  
  Your responsibilities:
  1. Create comprehensive test plans
  2. Design test cases covering all scenarios
  3. Execute functional and regression tests
  4. Report bugs with detailed reproduction steps
  5. Validate fixes and perform verification
  
  Testing Levels:
  - Unit Tests: Developer responsibility, QA validates coverage
  - Integration Tests: API contract testing
  - E2E Tests: User journey testing (Selenium/Playwright)
  - Performance Tests: Load and stress testing (k6)
  - Security Tests: OWASP compliance
  
  Test Case Format:
  ```
  TC-{ID}: {Title}
  Priority: Critical/High/Medium/Low
  Preconditions: [setup required]
  Steps:
    1. [action]
    2. [action]
  Expected Result: [outcome]
  Actual Result: [outcome]
  Status: Pass/Fail
  ```

capabilities:
  - Test planning
  - Test case design
  - Test execution
  - Bug reporting

outputs:
  - Test plans
  - Test cases
  - Bug reports
  - Test execution reports
```

#### 4.3.2 Domain Expert Agent (Email Marketing)

```yaml
agent_id: domain-expert-email
name: "Email Marketing Expert Agent"
model: claude-opus-4-5-20250514
role: "Validates features from email marketing practitioner perspective"

system_prompt: |
  You are the Email Marketing Expert Agent for the Ignite Mailing Platform.
  
  Your expertise:
  - 15+ years email marketing experience
  - Managed campaigns sending 10M+ emails/day
  - Deep knowledge of Ongage, Mailchimp, HubSpot, MailWizz
  - ESP relationship management
  - List hygiene and segmentation strategies
  
  Your responsibilities:
  1. Validate features meet real-world email marketing needs
  2. Identify missing functionality based on industry standards
  3. Review UI/UX from practitioner perspective
  4. Suggest workflow optimizations
  5. Validate campaign builder functionality
  
  Key Validation Areas:
  - List management workflows
  - Campaign creation experience
  - Segmentation power and flexibility
  - Template editing ease
  - Reporting and analytics usefulness
  - A/B testing capabilities
  - Automation workflows

capabilities:
  - Feature validation
  - UX review
  - Workflow analysis
  - Industry benchmarking

outputs:
  - Feature validation reports
  - UX recommendations
  - Workflow improvements
  - Industry compliance checks
```

#### 4.3.3 Domain Expert Agent (Deliverability)

```yaml
agent_id: domain-expert-deliverability
name: "Deliverability Specialist Agent"
model: claude-opus-4-5-20250514
role: "Validates features from email deliverability perspective"

system_prompt: |
  You are the Deliverability Specialist Agent for the Ignite Mailing Platform.
  
  Your expertise:
  - ISP relationships and inbox placement
  - SPF, DKIM, DMARC configuration
  - IP warming strategies
  - Bounce handling best practices
  - Complaint feedback loops
  - Reputation monitoring
  - Throttling strategies per ISP
  
  Your responsibilities:
  1. Validate delivery server configuration options
  2. Review throttling and warmup implementations
  3. Ensure bounce handling meets industry standards
  4. Validate tracking pixel implementation
  5. Review suppression list handling
  
  Key Validation Areas:
  - ESP integration correctness
  - Throttling per domain (Gmail: 500/hr, Yahoo: 400/hr, etc.)
  - Warmup plan effectiveness
  - Bounce classification accuracy
  - FBL processing
  - Blacklist integration
  - Domain reputation monitoring

capabilities:
  - Deliverability validation
  - ESP configuration review
  - Throttling strategy review
  - Compliance checking

outputs:
  - Deliverability validation reports
  - Configuration recommendations
  - Throttling parameter recommendations
  - Compliance reports
```

---

## 5. Component Build Plan

### 5.1 Component Priority Matrix

| Priority | Component | Business Value | Complexity | Dependencies |
|----------|-----------|----------------|------------|--------------|
| P0 | Portal Foundation | High | Low | None |
| P0 | Authentication & Multi-tenant | Critical | Medium | Portal |
| P1 | List Management | Critical | Medium | Auth |
| P1 | Subscriber Management | Critical | Medium | Lists |
| P2 | Delivery Server Management | Critical | High | Auth |
| P2 | Template Management | High | Medium | Auth |
| P3 | Campaign Builder | Critical | High | Lists, Templates, Servers |
| P3 | Segmentation Engine | High | High | Lists, Subscribers |
| P4 | Sending Engine & Workers | Critical | Very High | Campaigns, Servers |
| P4 | Tracking System | Critical | High | Campaigns |
| P5 | Bounce & FBL Processing | Critical | High | Tracking |
| P5 | Autoresponders | High | Medium | Campaigns |
| P6 | AI Optimization | High | High | Tracking, Subscribers |
| P6 | Transactional API | Medium | Medium | Servers |
| P7 | Analytics Integration | High | Medium | All |
| P7 | Reporting & Exports | Medium | Medium | All |

### 5.2 Component Specifications

#### Component 1: Portal Foundation

```yaml
component_id: C001
name: "Portal Foundation"
priority: P0
status: Pending

description: |
  Create the dual-portal landing page that allows users to switch between
  Analytics (existing) and Mailing (new) portals.

user_stories:
  - id: US-001
    title: "Portal Selection"
    story: |
      AS A logged-in user
      I WANT to see a portal selection page
      SO THAT I can choose between Analytics and Mailing
    acceptance_criteria:
      - Two portal cards displayed (Analytics, Mailing)
      - Visual distinction between portals
      - Click navigates to selected portal
      - Remember last selection (localStorage)
  
  - id: US-002
    title: "Portal Navigation"
    story: |
      AS A user in a portal
      I WANT to switch to the other portal
      SO THAT I can access different functionality
    acceptance_criteria:
      - Portal switcher in header
      - Smooth transition between portals
      - State preserved in each portal

technical_requirements:
  frontend:
    - PortalSelector.tsx component
    - PortalContext for state management
    - Route structure update
  backend:
    - No backend changes required

test_cases:
  - TC-001: Portal cards render correctly
  - TC-002: Analytics portal click navigates correctly
  - TC-003: Mailing portal click navigates correctly
  - TC-004: Portal switcher works from header
  - TC-005: Last selection persists on refresh

quality_gates:
  code_coverage: ">= 90%"
  unit_tests: "100% pass"
  e2e_tests: "100% pass"
  business_validation: "Product Owner approval"

estimated_effort: "2 days"
```

#### Component 2: List Management

```yaml
component_id: C002
name: "List Management"
priority: P1
status: Pending

description: |
  Complete mailing list management system including CRUD operations,
  custom fields, list settings (opt-in/out), and company information.

features:
  - Create, read, update, delete mailing lists
  - Custom field definitions (text, dropdown, checkbox, date, etc.)
  - List settings (single/double opt-in, opt-out behavior)
  - Company information per list
  - List statistics (subscriber counts by status)
  - List copy functionality
  - Bulk list operations

user_stories:
  - id: US-010
    title: "Create Mailing List"
    story: |
      AS A user
      I WANT to create a new mailing list
      SO THAT I can organize my subscribers
    acceptance_criteria:
      - Form with name, display name, description
      - Opt-in type selection (single/double)
      - Opt-out type selection
      - Welcome email toggle
      - Save creates list and redirects to list detail

  - id: US-011
    title: "Manage Custom Fields"
    story: |
      AS A user
      I WANT to add custom fields to my list
      SO THAT I can collect additional subscriber data
    acceptance_criteria:
      - Add field with type selection
      - Field types: text, textarea, dropdown, checkbox, date, number, phone, URL
      - Set required/optional
      - Set default value
      - Reorder fields
      - Delete fields (with confirmation)

  - id: US-012
    title: "View List Statistics"
    story: |
      AS A user
      I WANT to see my list statistics
      SO THAT I can understand my subscriber base
    acceptance_criteria:
      - Total subscribers
      - Confirmed/unconfirmed counts
      - Unsubscribed count
      - Blacklisted count
      - Growth chart (last 30 days)

technical_requirements:
  backend:
    files:
      - internal/mailing/lists/service.go
      - internal/mailing/lists/repository.go
      - internal/mailing/lists/handlers.go
      - internal/mailing/lists/types.go
      - internal/mailing/lists/service_test.go
      - internal/mailing/lists/handlers_test.go
    api_endpoints:
      - GET /api/mailing/lists
      - POST /api/mailing/lists
      - GET /api/mailing/lists/{uid}
      - PUT /api/mailing/lists/{uid}
      - DELETE /api/mailing/lists/{uid}
      - GET /api/mailing/lists/{uid}/stats
      - POST /api/mailing/lists/{uid}/copy
      - GET /api/mailing/lists/{uid}/fields
      - POST /api/mailing/lists/{uid}/fields
      - PUT /api/mailing/lists/{uid}/fields/{fieldId}
      - DELETE /api/mailing/lists/{uid}/fields/{fieldId}
  
  frontend:
    files:
      - web/src/components/mailing/lists/ListsDashboard.tsx
      - web/src/components/mailing/lists/ListForm.tsx
      - web/src/components/mailing/lists/ListDetails.tsx
      - web/src/components/mailing/lists/ListFields.tsx
      - web/src/components/mailing/lists/ListStats.tsx
      - web/src/components/mailing/lists/index.ts
      - web/src/components/mailing/lists/*.test.tsx

  database:
    tables:
      - mailing_lists
      - mailing_list_fields
      - mailing_list_field_options
      - mailing_list_company

test_cases:
  functional:
    - TC-010: Create list with valid data
    - TC-011: Create list validation errors
    - TC-012: Update list
    - TC-013: Delete list (no subscribers)
    - TC-014: Delete list (has subscribers) - confirm dialog
    - TC-015: Add custom text field
    - TC-016: Add dropdown field with options
    - TC-017: Reorder fields
    - TC-018: Delete field
    - TC-019: Copy list
    - TC-020: View list statistics
  
  integration:
    - TC-021: API returns paginated lists
    - TC-022: API creates list with fields
    - TC-023: API validates unique list name
  
  e2e:
    - TC-024: Full list creation workflow
    - TC-025: Field management workflow

quality_gates:
  code_coverage: ">= 90%"
  unit_tests: "100% pass"
  integration_tests: "100% pass"
  e2e_tests: "100% pass"
  domain_expert_review: "Email Marketing Expert approval"

estimated_effort: "5 days"
```

[Continue with all remaining components...]

---

## 6. Technical Architecture

### 6.1 System Overview

```
┌─────────────────────────────────────────────────────────────────────────────────────────┐
│                              LOAD BALANCER (ALB)                                         │
│                                  Port 80/443                                             │
└─────────────────────────────────────┬───────────────────────────────────────────────────┘
                                      │
┌─────────────────────────────────────▼───────────────────────────────────────────────────┐
│                              API GATEWAY (Traefik)                                       │
│                         Rate Limiting, Auth, Routing                                     │
└────────────┬──────────────────────┬──────────────────────┬──────────────────────────────┘
             │                      │                      │
    ┌────────▼────────┐    ┌───────▼───────┐    ┌────────▼────────┐
    │   ANALYTICS     │    │    MAILING    │    │    TRACKING     │
    │   SERVICE       │    │    API        │    │    SERVICE      │
    │   (Go)          │    │    (Go)       │    │    (Go)         │
    │   Replicas: 2   │    │   Replicas: 3 │    │   Replicas: 5   │
    └────────┬────────┘    └───────┬───────┘    └────────┬────────┘
             │                     │                     │
             └──────────────┬──────┴─────────────────────┘
                            │
┌───────────────────────────▼─────────────────────────────────────────────────────────────┐
│                              MESSAGE QUEUE (Redis Streams)                               │
│                                                                                          │
│  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐   │
│  │ send:high   │  │ send:normal │  │ send:low    │  │  tracking   │  │   bounce    │   │
│  │ (priority)  │  │             │  │             │  │             │  │             │   │
│  └─────────────┘  └─────────────┘  └─────────────┘  └─────────────┘  └─────────────┘   │
└─────────────────────────────────────┬───────────────────────────────────────────────────┘
                                      │
         ┌────────────────────────────┼────────────────────────────┐
         │                            │                            │
┌────────▼────────┐          ┌───────▼───────┐          ┌─────────▼─────────┐
│   SEND WORKER   │          │ BOUNCE WORKER │          │  TRACKING WORKER  │
│   (Go)          │          │   (Go)        │          │    (Go)           │
│   Replicas: 20  │          │  Replicas: 2  │          │   Replicas: 3     │
└────────┬────────┘          └───────┬───────┘          └─────────┬─────────┘
         │                           │                            │
         └───────────────────────────┼────────────────────────────┘
                                     │
┌────────────────────────────────────▼────────────────────────────────────────────────────┐
│                                    DATA LAYER                                            │
│                                                                                          │
│  ┌─────────────────┐  ┌─────────────────┐  ┌─────────────────┐  ┌─────────────────┐    │
│  │   PostgreSQL    │  │     Redis       │  │    DynamoDB     │  │       S3        │    │
│  │   (RDS)         │  │  (ElastiCache)  │  │                 │  │                 │    │
│  │                 │  │                 │  │                 │  │                 │    │
│  │ • Lists         │  │ • Session       │  │ • Delivery logs │  │ • Templates     │    │
│  │ • Campaigns     │  │ • Rate limits   │  │ • Tracking      │  │ • Attachments   │    │
│  │ • Subscribers   │  │ • Queue         │  │ • Metrics       │  │ • Exports       │    │
│  │ • Segments      │  │ • Pub/Sub       │  │                 │  │                 │    │
│  └─────────────────┘  └─────────────────┘  └─────────────────┘  └─────────────────┘    │
└─────────────────────────────────────────────────────────────────────────────────────────┘
                                     │
┌────────────────────────────────────▼────────────────────────────────────────────────────┐
│                              EXTERNAL SERVICES (ESPs)                                    │
│                                                                                          │
│  ┌─────────────────────────────────────────────────────────────────────────────────┐   │
│  │                           ESP ROUTER (Intelligent)                               │   │
│  │  • Load balancing across ESPs                                                    │   │
│  │  • Failover on errors                                                           │   │
│  │  • Domain-level routing rules                                                   │   │
│  │  • Quota management                                                             │   │
│  └─────────────────────────────────────────────────────────────────────────────────┘   │
│                                      │                                                  │
│         ┌────────────────────────────┼────────────────────────────┐                    │
│         │                            │                            │                    │
│  ┌──────▼──────┐              ┌──────▼──────┐              ┌──────▼──────┐            │
│  │  SparkPost  │              │   AWS SES   │              │   Mailgun   │            │
│  │             │              │             │              │  (Future)   │            │
│  │ Accounts:   │              │ Accounts:   │              │             │            │
│  │ • Primary   │              │ • us-west-2 │              │             │            │
│  │ • Secondary │              │ • us-east-1 │              │             │            │
│  └─────────────┘              └─────────────┘              └─────────────┘            │
└─────────────────────────────────────────────────────────────────────────────────────────┘
```

### 6.2 Service Definitions

| Service | Port | Replicas | CPU | Memory | Purpose |
|---------|------|----------|-----|--------|---------|
| `api-gateway` | 8080 | 2-5 | 1 vCPU | 1 GB | Main API, routing |
| `analytics-service` | 8081 | 2 | 0.5 vCPU | 512 MB | ESP metrics, dashboards |
| `mailing-api` | 8082 | 3-10 | 1 vCPU | 1 GB | Mailing CRUD operations |
| `tracking-service` | 8083 | 5-30 | 0.5 vCPU | 512 MB | Open/click/unsub tracking |
| `send-worker` | - | 10-50 | 2 vCPU | 2 GB | Email delivery |
| `bounce-worker` | - | 2-5 | 0.5 vCPU | 512 MB | Bounce processing |
| `tracking-worker` | - | 3-10 | 0.5 vCPU | 512 MB | Event aggregation |
| `ai-worker` | - | 2-5 | 1 vCPU | 1 GB | ML scoring |
| `scheduler` | - | 1 | 0.5 vCPU | 512 MB | Campaign scheduling |
| `collector` | - | 1 | 0.5 vCPU | 512 MB | ESP metrics collection |

---

## 7. Docker Microservices Architecture

### 7.1 Docker Compose (Development)

```yaml
version: '3.9'

services:
  # ==================== API GATEWAY ====================
  traefik:
    image: traefik:v2.10
    container_name: ignite-traefik
    command:
      - "--api.dashboard=true"
      - "--providers.docker=true"
      - "--providers.docker.exposedbydefault=false"
      - "--entrypoints.web.address=:80"
      - "--entrypoints.websecure.address=:443"
      - "--metrics.prometheus=true"
    ports:
      - "80:80"
      - "443:443"
      - "8080:8080"
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
    networks:
      - ignite-network
    restart: unless-stopped

  # ==================== FRONTEND ====================
  frontend:
    build:
      context: ./web
      dockerfile: Dockerfile
    container_name: ignite-frontend
    labels:
      - "traefik.enable=true"
      - "traefik.http.routers.frontend.rule=PathPrefix(`/`)"
      - "traefik.http.routers.frontend.priority=1"
      - "traefik.http.services.frontend.loadbalancer.server.port=3000"
    environment:
      - VITE_API_URL=http://api-gateway:8080
    depends_on:
      - api-gateway
    networks:
      - ignite-network

  # ==================== API SERVICES ====================
  api-gateway:
    build:
      context: .
      dockerfile: docker/Dockerfile.api-gateway
    container_name: ignite-api-gateway
    labels:
      - "traefik.enable=true"
      - "traefik.http.routers.api.rule=PathPrefix(`/api`) || PathPrefix(`/auth`) || PathPrefix(`/health`)"
      - "traefik.http.routers.api.priority=10"
      - "traefik.http.services.api.loadbalancer.server.port=8080"
    environment:
      - CONFIG_PATH=/app/config/config.yaml
      - REDIS_URL=redis://redis:6379
      - POSTGRES_URL=postgres://ignite:ignite@postgres:5432/ignite?sslmode=disable
      - DYNAMODB_ENDPOINT=http://dynamodb-local:8000
    volumes:
      - ./config:/app/config:ro
    depends_on:
      - redis
      - postgres
      - dynamodb-local
    networks:
      - ignite-network
    deploy:
      replicas: 2

  mailing-api:
    build:
      context: .
      dockerfile: docker/Dockerfile.mailing-api
    environment:
      - SERVICE_NAME=mailing-api
      - CONFIG_PATH=/app/config/config.yaml
      - REDIS_URL=redis://redis:6379
      - POSTGRES_URL=postgres://ignite:ignite@postgres:5432/ignite?sslmode=disable
    volumes:
      - ./config:/app/config:ro
    depends_on:
      - redis
      - postgres
    networks:
      - ignite-network
    deploy:
      replicas: 3

  tracking-service:
    build:
      context: .
      dockerfile: docker/Dockerfile.tracking
    labels:
      - "traefik.enable=true"
      - "traefik.http.routers.tracking.rule=PathPrefix(`/track`) || PathPrefix(`/unsubscribe`)"
      - "traefik.http.routers.tracking.priority=20"
      - "traefik.http.services.tracking.loadbalancer.server.port=8081"
    environment:
      - SERVICE_NAME=tracking
      - CONFIG_PATH=/app/config/config.yaml
      - REDIS_URL=redis://redis:6379
      - DYNAMODB_ENDPOINT=http://dynamodb-local:8000
    volumes:
      - ./config:/app/config:ro
    depends_on:
      - redis
      - dynamodb-local
    networks:
      - ignite-network
    deploy:
      replicas: 5

  # ==================== WORKERS ====================
  send-worker:
    build:
      context: .
      dockerfile: docker/Dockerfile.send-worker
    environment:
      - SERVICE_NAME=send-worker
      - CONFIG_PATH=/app/config/config.yaml
      - REDIS_URL=redis://redis:6379
      - POSTGRES_URL=postgres://ignite:ignite@postgres:5432/ignite?sslmode=disable
      - WORKER_CONCURRENCY=50
      - BATCH_SIZE=100
    volumes:
      - ./config:/app/config:ro
    depends_on:
      - redis
      - postgres
    networks:
      - ignite-network
    deploy:
      replicas: 10

  bounce-worker:
    build:
      context: .
      dockerfile: docker/Dockerfile.bounce-worker
    environment:
      - SERVICE_NAME=bounce-worker
      - CONFIG_PATH=/app/config/config.yaml
      - REDIS_URL=redis://redis:6379
      - POSTGRES_URL=postgres://ignite:ignite@postgres:5432/ignite?sslmode=disable
    volumes:
      - ./config:/app/config:ro
    depends_on:
      - redis
      - postgres
    networks:
      - ignite-network
    deploy:
      replicas: 2

  tracking-worker:
    build:
      context: .
      dockerfile: docker/Dockerfile.tracking-worker
    environment:
      - SERVICE_NAME=tracking-worker
      - CONFIG_PATH=/app/config/config.yaml
      - REDIS_URL=redis://redis:6379
      - DYNAMODB_ENDPOINT=http://dynamodb-local:8000
    volumes:
      - ./config:/app/config:ro
    depends_on:
      - redis
      - dynamodb-local
    networks:
      - ignite-network
    deploy:
      replicas: 3

  ai-worker:
    build:
      context: .
      dockerfile: docker/Dockerfile.ai-worker
    environment:
      - SERVICE_NAME=ai-worker
      - CONFIG_PATH=/app/config/config.yaml
      - REDIS_URL=redis://redis:6379
      - POSTGRES_URL=postgres://ignite:ignite@postgres:5432/ignite?sslmode=disable
      - OPENAI_API_KEY=${OPENAI_API_KEY}
    volumes:
      - ./config:/app/config:ro
    depends_on:
      - redis
      - postgres
    networks:
      - ignite-network
    deploy:
      replicas: 2

  scheduler:
    build:
      context: .
      dockerfile: docker/Dockerfile.scheduler
    container_name: ignite-scheduler
    environment:
      - SERVICE_NAME=scheduler
      - CONFIG_PATH=/app/config/config.yaml
      - REDIS_URL=redis://redis:6379
      - POSTGRES_URL=postgres://ignite:ignite@postgres:5432/ignite?sslmode=disable
    volumes:
      - ./config:/app/config:ro
    depends_on:
      - redis
      - postgres
    networks:
      - ignite-network

  # ==================== DATA LAYER ====================
  postgres:
    image: postgres:16-alpine
    container_name: ignite-postgres
    environment:
      - POSTGRES_USER=ignite
      - POSTGRES_PASSWORD=ignite
      - POSTGRES_DB=ignite
    volumes:
      - postgres-data:/var/lib/postgresql/data
      - ./docker/init-db.sql:/docker-entrypoint-initdb.d/init.sql:ro
    ports:
      - "5432:5432"
    networks:
      - ignite-network

  redis:
    image: redis:7-alpine
    container_name: ignite-redis
    command: redis-server --appendonly yes --maxmemory 1gb --maxmemory-policy allkeys-lru
    volumes:
      - redis-data:/data
    ports:
      - "6379:6379"
    networks:
      - ignite-network

  dynamodb-local:
    image: amazon/dynamodb-local:latest
    container_name: ignite-dynamodb
    command: "-jar DynamoDBLocal.jar -sharedDb -dbPath /data"
    volumes:
      - dynamodb-data:/data
    ports:
      - "8000:8000"
    networks:
      - ignite-network

  # ==================== MONITORING ====================
  prometheus:
    image: prom/prometheus:latest
    container_name: ignite-prometheus
    volumes:
      - ./docker/prometheus.yml:/etc/prometheus/prometheus.yml:ro
      - prometheus-data:/prometheus
    ports:
      - "9090:9090"
    networks:
      - ignite-network

  grafana:
    image: grafana/grafana:latest
    container_name: ignite-grafana
    environment:
      - GF_SECURITY_ADMIN_PASSWORD=ignite
      - GF_USERS_ALLOW_SIGN_UP=false
    volumes:
      - grafana-data:/var/lib/grafana
      - ./docker/grafana/dashboards:/etc/grafana/provisioning/dashboards:ro
      - ./docker/grafana/datasources:/etc/grafana/provisioning/datasources:ro
    ports:
      - "3001:3000"
    networks:
      - ignite-network

networks:
  ignite-network:
    driver: bridge

volumes:
  postgres-data:
  redis-data:
  dynamodb-data:
  prometheus-data:
  grafana-data:
```

### 7.2 Capacity Planning

| Metric | Value | Calculation |
|--------|-------|-------------|
| **Daily Volume** | 8,000,000 | Target |
| **Peak Hour** | 1,000,000 | ~12.5% of daily |
| **Peak/Second** | 278 | 1M / 3600 |
| **Target/Worker** | 30/sec | Conservative |
| **Workers Needed** | 10 | 278 / 30 |
| **Buffer (2x)** | 20 | Burst capacity |

---

## 8. Database Schema

### 8.1 PostgreSQL Schema

```sql
-- Lists
CREATE TABLE mailing_lists (
    id BIGSERIAL PRIMARY KEY,
    uid VARCHAR(36) UNIQUE NOT NULL DEFAULT gen_random_uuid(),
    customer_id BIGINT NOT NULL,
    name VARCHAR(255) NOT NULL,
    display_name VARCHAR(255),
    description TEXT,
    visibility VARCHAR(20) DEFAULT 'private',
    opt_in VARCHAR(20) DEFAULT 'double',
    opt_out VARCHAR(20) DEFAULT 'single',
    welcome_email BOOLEAN DEFAULT false,
    subscriber_require_approval BOOLEAN DEFAULT false,
    status VARCHAR(20) DEFAULT 'active',
    subscriber_count INTEGER DEFAULT 0,
    confirmed_count INTEGER DEFAULT 0,
    unsubscribed_count INTEGER DEFAULT 0,
    blacklisted_count INTEGER DEFAULT 0,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

-- List Custom Fields
CREATE TABLE mailing_list_fields (
    id BIGSERIAL PRIMARY KEY,
    list_id BIGINT REFERENCES mailing_lists(id) ON DELETE CASCADE,
    label VARCHAR(255) NOT NULL,
    tag VARCHAR(50) NOT NULL,
    field_type VARCHAR(50) NOT NULL,
    default_value TEXT,
    required BOOLEAN DEFAULT false,
    visible BOOLEAN DEFAULT true,
    sort_order INTEGER DEFAULT 0,
    validation_rules JSONB,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

-- Subscribers
CREATE TABLE mailing_subscribers (
    id BIGSERIAL PRIMARY KEY,
    uid VARCHAR(36) UNIQUE NOT NULL DEFAULT gen_random_uuid(),
    list_id BIGINT REFERENCES mailing_lists(id) ON DELETE CASCADE,
    email VARCHAR(255) NOT NULL,
    email_hash VARCHAR(64) NOT NULL,
    status VARCHAR(20) DEFAULT 'unconfirmed',
    source VARCHAR(20) DEFAULT 'web',
    ip_address VARCHAR(45),
    timezone VARCHAR(50),
    engagement_score DECIMAL(5,2) DEFAULT 50.00,
    optimal_send_hour SMALLINT,
    last_open_at TIMESTAMP,
    last_click_at TIMESTAMP,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(list_id, email)
);

CREATE INDEX idx_subscribers_email_hash ON mailing_subscribers(email_hash);
CREATE INDEX idx_subscribers_status ON mailing_subscribers(list_id, status);

-- Subscriber Field Values
CREATE TABLE mailing_subscriber_field_values (
    id BIGSERIAL PRIMARY KEY,
    subscriber_id BIGINT REFERENCES mailing_subscribers(id) ON DELETE CASCADE,
    field_id BIGINT REFERENCES mailing_list_fields(id) ON DELETE CASCADE,
    value TEXT,
    UNIQUE(subscriber_id, field_id)
);

-- Campaigns
CREATE TABLE mailing_campaigns (
    id BIGSERIAL PRIMARY KEY,
    uid VARCHAR(36) UNIQUE NOT NULL DEFAULT gen_random_uuid(),
    customer_id BIGINT NOT NULL,
    list_id BIGINT REFERENCES mailing_lists(id),
    segment_id BIGINT,
    template_id BIGINT,
    type VARCHAR(20) DEFAULT 'regular',
    name VARCHAR(255) NOT NULL,
    subject VARCHAR(500) NOT NULL,
    from_name VARCHAR(255) NOT NULL,
    from_email VARCHAR(255) NOT NULL,
    reply_to VARCHAR(255),
    preheader VARCHAR(255),
    status VARCHAR(30) DEFAULT 'draft',
    send_at TIMESTAMP,
    started_at TIMESTAMP,
    finished_at TIMESTAMP,
    total_recipients INTEGER DEFAULT 0,
    processed_count INTEGER DEFAULT 0,
    delivered_count INTEGER DEFAULT 0,
    bounced_count INTEGER DEFAULT 0,
    opened_count INTEGER DEFAULT 0,
    clicked_count INTEGER DEFAULT 0,
    unsubscribed_count INTEGER DEFAULT 0,
    complained_count INTEGER DEFAULT 0,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_campaigns_status ON mailing_campaigns(status, send_at);

-- Templates
CREATE TABLE mailing_templates (
    id BIGSERIAL PRIMARY KEY,
    uid VARCHAR(36) UNIQUE NOT NULL DEFAULT gen_random_uuid(),
    customer_id BIGINT NOT NULL,
    category_id BIGINT,
    name VARCHAR(255) NOT NULL,
    content_html TEXT,
    content_plain TEXT,
    thumbnail_url VARCHAR(500),
    is_system BOOLEAN DEFAULT false,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

-- Segments
CREATE TABLE mailing_segments (
    id BIGSERIAL PRIMARY KEY,
    uid VARCHAR(36) UNIQUE NOT NULL DEFAULT gen_random_uuid(),
    list_id BIGINT REFERENCES mailing_lists(id) ON DELETE CASCADE,
    name VARCHAR(255) NOT NULL,
    operator_match VARCHAR(10) DEFAULT 'all',
    status VARCHAR(20) DEFAULT 'active',
    subscriber_count INTEGER DEFAULT 0,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

-- Segment Conditions
CREATE TABLE mailing_segment_conditions (
    id BIGSERIAL PRIMARY KEY,
    segment_id BIGINT REFERENCES mailing_segments(id) ON DELETE CASCADE,
    field_id BIGINT,
    operator VARCHAR(30) NOT NULL,
    value TEXT,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

-- Delivery Servers
CREATE TABLE mailing_delivery_servers (
    id BIGSERIAL PRIMARY KEY,
    uid VARCHAR(36) UNIQUE NOT NULL DEFAULT gen_random_uuid(),
    customer_id BIGINT,
    name VARCHAR(255) NOT NULL,
    type VARCHAR(50) NOT NULL,
    hostname VARCHAR(255),
    port INTEGER DEFAULT 587,
    username VARCHAR(255),
    password_encrypted TEXT,
    encryption VARCHAR(10) DEFAULT 'tls',
    from_email VARCHAR(255),
    from_name VARCHAR(255),
    hourly_quota INTEGER DEFAULT 0,
    daily_quota INTEGER DEFAULT 0,
    monthly_quota INTEGER DEFAULT 0,
    probability INTEGER DEFAULT 100,
    warmup_plan_id BIGINT,
    status VARCHAR(20) DEFAULT 'active',
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

-- Bounce Servers
CREATE TABLE mailing_bounce_servers (
    id BIGSERIAL PRIMARY KEY,
    uid VARCHAR(36) UNIQUE NOT NULL DEFAULT gen_random_uuid(),
    customer_id BIGINT,
    name VARCHAR(255) NOT NULL,
    hostname VARCHAR(255) NOT NULL,
    port INTEGER DEFAULT 993,
    username VARCHAR(255) NOT NULL,
    password_encrypted TEXT NOT NULL,
    protocol VARCHAR(10) DEFAULT 'imap',
    encryption VARCHAR(10) DEFAULT 'ssl',
    validate_ssl BOOLEAN DEFAULT true,
    search_charset VARCHAR(20) DEFAULT 'UTF-8',
    delete_all_messages BOOLEAN DEFAULT false,
    status VARCHAR(20) DEFAULT 'active',
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

-- Suppression Lists
CREATE TABLE mailing_suppression_lists (
    id BIGSERIAL PRIMARY KEY,
    uid VARCHAR(36) UNIQUE NOT NULL DEFAULT gen_random_uuid(),
    customer_id BIGINT NOT NULL,
    name VARCHAR(255) NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE mailing_suppression_emails (
    id BIGSERIAL PRIMARY KEY,
    suppression_list_id BIGINT REFERENCES mailing_suppression_lists(id) ON DELETE CASCADE,
    email VARCHAR(255) NOT NULL,
    email_hash VARCHAR(64) NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(suppression_list_id, email_hash)
);

-- Blacklist
CREATE TABLE mailing_blacklist (
    id BIGSERIAL PRIMARY KEY,
    email VARCHAR(255) UNIQUE NOT NULL,
    email_hash VARCHAR(64) UNIQUE NOT NULL,
    reason VARCHAR(50),
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
```

### 8.2 DynamoDB Tables (High-Volume)

```yaml
# Delivery Logs
DeliveryLogsTable:
  TableName: ignite-delivery-logs
  KeySchema:
    - AttributeName: PK  # CAMPAIGN#{uid}
      KeyType: HASH
    - AttributeName: SK  # LOG#{timestamp}#{subscriber_uid}
      KeyType: RANGE
  AttributeDefinitions:
    - AttributeName: PK
      AttributeType: S
    - AttributeName: SK
      AttributeType: S
  TimeToLiveSpecification:
    AttributeName: ttl
    Enabled: true
  BillingMode: PAY_PER_REQUEST

# Tracking Events
TrackingEventsTable:
  TableName: ignite-tracking-events
  KeySchema:
    - AttributeName: PK  # CAMPAIGN#{uid}
      KeyType: HASH
    - AttributeName: SK  # EVENT#{type}#{timestamp}#{subscriber_uid}
      KeyType: RANGE
  TimeToLiveSpecification:
    AttributeName: ttl
    Enabled: true
  BillingMode: PAY_PER_REQUEST

# Engagement Metrics
EngagementMetricsTable:
  TableName: ignite-engagement-metrics
  KeySchema:
    - AttributeName: PK  # SUBSCRIBER#{uid}
      KeyType: HASH
    - AttributeName: SK  # ENGAGEMENT
      KeyType: RANGE
  BillingMode: PAY_PER_REQUEST
```

---

## 9. API Specification

### 9.1 Mailing Portal API Endpoints

```yaml
openapi: 3.0.3
info:
  title: Ignite Mailing Platform API
  version: 1.0.0

paths:
  # Lists
  /api/mailing/lists:
    get:
      summary: Get all lists
      parameters:
        - name: page
          in: query
          schema:
            type: integer
            default: 1
        - name: per_page
          in: query
          schema:
            type: integer
            default: 20
        - name: status
          in: query
          schema:
            type: string
            enum: [active, archived]
      responses:
        200:
          description: Paginated list of mailing lists
    post:
      summary: Create a new list
      requestBody:
        content:
          application/json:
            schema:
              $ref: '#/components/schemas/CreateListRequest'
      responses:
        201:
          description: List created

  /api/mailing/lists/{uid}:
    get:
      summary: Get list details
    put:
      summary: Update list
    delete:
      summary: Delete list

  /api/mailing/lists/{uid}/stats:
    get:
      summary: Get list statistics

  # Subscribers
  /api/mailing/lists/{listUid}/subscribers:
    get:
      summary: Get subscribers
    post:
      summary: Add subscriber

  /api/mailing/subscribers/import:
    post:
      summary: Import subscribers from CSV
      requestBody:
        content:
          multipart/form-data:
            schema:
              type: object
              properties:
                file:
                  type: string
                  format: binary
                list_uid:
                  type: string

  # Campaigns
  /api/mailing/campaigns:
    get:
      summary: Get all campaigns
    post:
      summary: Create campaign

  /api/mailing/campaigns/{uid}/send:
    post:
      summary: Start sending campaign

  /api/mailing/campaigns/{uid}/stats:
    get:
      summary: Get campaign statistics

  # Templates
  /api/mailing/templates:
    get:
      summary: Get all templates
    post:
      summary: Create template

  # Segments
  /api/mailing/segments:
    get:
      summary: Get all segments
    post:
      summary: Create segment

  /api/mailing/segments/{uid}/count:
    get:
      summary: Get segment subscriber count

  # Delivery Servers
  /api/mailing/servers:
    get:
      summary: Get delivery servers
    post:
      summary: Add delivery server

  /api/mailing/servers/{uid}/test:
    post:
      summary: Test delivery server connection

  # Tracking (Public - No Auth)
  /track/open/{campaignUid}/{subscriberUid}:
    get:
      summary: Track email open
      security: []

  /track/click/{campaignUid}/{subscriberUid}/{urlHash}:
    get:
      summary: Track link click
      security: []

components:
  schemas:
    CreateListRequest:
      type: object
      required:
        - name
      properties:
        name:
          type: string
          maxLength: 255
        display_name:
          type: string
        description:
          type: string
        opt_in:
          type: string
          enum: [single, double]
          default: double
        opt_out:
          type: string
          enum: [single, double]
          default: single
        welcome_email:
          type: boolean
          default: false
```

---

## 10. Quality Gates

### 10.1 Component Completion Checklist

```markdown
## Component: [Name]
### Code Quality
- [ ] Code coverage ≥ 90%
- [ ] All unit tests passing
- [ ] All integration tests passing
- [ ] No linting errors
- [ ] Code reviewed and approved

### Security
- [ ] Security scan clean (0 critical/high)
- [ ] Input validation implemented
- [ ] SQL injection prevention
- [ ] XSS prevention
- [ ] Authentication/authorization verified

### Performance
- [ ] API response time p99 < 200ms
- [ ] Database queries optimized
- [ ] Caching implemented where appropriate
- [ ] Load test passed (target throughput)

### Documentation
- [ ] API documentation complete
- [ ] Code comments for complex logic
- [ ] README updated

### Business Validation
- [ ] Product Owner sign-off
- [ ] User stories acceptance criteria met
- [ ] Domain expert review passed (Email Marketing)
- [ ] Domain expert review passed (Deliverability)

### Enterprise Ready
- [ ] Error handling comprehensive
- [ ] Logging implemented
- [ ] Monitoring metrics exposed
- [ ] Graceful degradation
- [ ] Rollback plan documented
```

### 10.2 Release Checklist

```markdown
## Release: v[X.Y.Z]
### Pre-Release
- [ ] All component checklists complete
- [ ] E2E test suite passed
- [ ] Performance benchmarks met
- [ ] Security audit passed
- [ ] Database migrations tested
- [ ] Rollback procedure tested

### Deployment
- [ ] Docker images built and tested
- [ ] Environment variables configured
- [ ] Secrets rotated if needed
- [ ] Database backup taken
- [ ] Monitoring alerts configured

### Post-Release
- [ ] Smoke tests passed
- [ ] Metrics within expected range
- [ ] No error spikes
- [ ] User feedback positive
```

---

## 11. Implementation Phases

### Phase 1: Foundation (Week 1-2)
- [ ] C001: Portal Foundation
- [ ] C002: Authentication & Multi-tenant
- [ ] Docker infrastructure setup
- [ ] CI/CD pipeline setup

### Phase 2: Core Data (Week 3-4)
- [ ] C003: List Management
- [ ] C004: Subscriber Management
- [ ] Database schema implementation
- [ ] Import/Export functionality

### Phase 3: Delivery Infrastructure (Week 5-6)
- [ ] C005: Delivery Server Management
- [ ] C006: Template Management
- [ ] SparkPost integration
- [ ] AWS SES integration

### Phase 4: Campaign Engine (Week 7-8)
- [ ] C007: Campaign Builder
- [ ] C008: Segmentation Engine
- [ ] Queue infrastructure
- [ ] Send workers

### Phase 5: Tracking & Processing (Week 9-10)
- [ ] C009: Tracking System
- [ ] C010: Bounce & FBL Processing
- [ ] Event aggregation
- [ ] Blacklist management

### Phase 6: Advanced Features (Week 11-12)
- [ ] C011: Autoresponders
- [ ] C012: Transactional API
- [ ] C013: AI Optimization
- [ ] Analytics integration

### Phase 7: Polish & Launch (Week 13-14)
- [ ] C014: Reporting & Exports
- [ ] Performance optimization
- [ ] Security hardening
- [ ] Documentation
- [ ] Production deployment

---

## 12. Appendices

### Appendix A: Agent Invocation Protocol

```yaml
# MCP Tool Invocation for Agents
agents:
  invoke_agent:
    description: "Invoke a specific agent for a task"
    parameters:
      agent_id:
        type: string
        enum: [business-opportunity, product-owner, business-analyst,
               solutions-architect, backend-developer, frontend-developer,
               devops-engineer, qa-lead, domain-expert-email, domain-expert-deliverability]
      task:
        type: string
        description: "The task to perform"
      component_id:
        type: string
        description: "The component being worked on"
      context:
        type: object
        description: "Additional context for the agent"
```

### Appendix B: Component Status Tracking

| Component | Status | Owner | Started | Completed | Quality Score |
|-----------|--------|-------|---------|-----------|---------------|
| C001 Portal Foundation | Pending | - | - | - | - |
| C002 Authentication | Pending | - | - | - | - |
| C003 List Management | Pending | - | - | - | - |
| ... | ... | ... | ... | ... | ... |

### Appendix C: Risk Register

| Risk | Impact | Probability | Mitigation |
|------|--------|-------------|------------|
| ESP rate limiting | High | Medium | Adaptive throttling, multi-account |
| Data loss | Critical | Low | Backups, replication |
| Performance degradation | High | Medium | Auto-scaling, caching |
| Security breach | Critical | Low | Security audit, encryption |

---

**Document End**

*This document serves as the complete blueprint for building the Ignite Mailing Platform using an agentic framework powered by Claude Opus 4.5.*
