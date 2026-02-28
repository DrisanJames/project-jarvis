# Component Build Specifications

**Version:** 1.0.0  
**Purpose:** Detailed specifications for each platform component  

---

## Component Registry

| ID | Component | Priority | Est. Days | Dependencies |
|----|-----------|----------|-----------|--------------|
| C001 | Portal Foundation | P0 | 2 | None |
| C002 | Multi-tenant Auth | P0 | 3 | C001 |
| C003 | List Management | P1 | 5 | C002 |
| C004 | Subscriber Management | P1 | 5 | C003 |
| C005 | Delivery Servers | P2 | 5 | C002 |
| C006 | Template Management | P2 | 4 | C002 |
| C007 | Campaign Builder | P3 | 7 | C003, C005, C006 |
| C008 | Segmentation Engine | P3 | 5 | C003, C004 |
| C009 | Sending Engine | P4 | 10 | C007, C008 |
| C010 | Tracking System | P4 | 5 | C007 |
| C011 | Bounce/FBL Processing | P5 | 5 | C009, C010 |
| C012 | Autoresponders | P5 | 4 | C007 |
| C013 | AI Optimization | P6 | 7 | C010 |
| C014 | Transactional API | P6 | 3 | C005 |

---

## C001: Portal Foundation

### Overview
```yaml
component_id: C001
name: "Portal Foundation"
priority: P0
estimated_days: 2
dependencies: []
```

### Business Requirements

**BR-001:** Users shall be presented with a portal selection screen upon login  
**BR-002:** Two portals shall be available: Analytics (existing) and Mailing (new)  
**BR-003:** Users shall be able to switch between portals without re-authentication  
**BR-004:** The system shall remember the user's last selected portal  

### Functional Requirements

**FR-001:** Portal Selection Page
- Display two portal cards with icons and descriptions
- Analytics portal: Chart icon, "ESP Performance, Revenue, Financials"
- Mailing portal: Email icon, "Lists, Campaigns, Templates, Delivery"
- Click navigates to selected portal

**FR-002:** Portal Switching
- Header component includes portal switcher
- Current portal is highlighted
- Switching preserves authentication state

**FR-003:** State Persistence
- Store last selected portal in localStorage
- Auto-navigate to last portal on subsequent visits

### Technical Design

#### Frontend Components
```
web/src/components/portal/
├── PortalSelector.tsx      # Main portal selection page
├── PortalCard.tsx          # Individual portal card
├── PortalSwitcher.tsx      # Header portal switcher
├── AnalyticsPortal.tsx     # Analytics portal wrapper
├── MailingPortal.tsx       # Mailing portal wrapper
├── types.ts
├── index.ts
└── *.test.tsx
```

#### PortalContext
```typescript
interface PortalContextType {
  currentPortal: 'analytics' | 'mailing' | null;
  setPortal: (portal: 'analytics' | 'mailing') => void;
  isPortalSelected: boolean;
}
```

#### App.tsx Changes
```typescript
// Add portal routing
const App: React.FC = () => {
  const { currentPortal, isPortalSelected } = usePortal();
  
  if (!isPortalSelected) {
    return <PortalSelector />;
  }
  
  return currentPortal === 'analytics' 
    ? <AnalyticsPortal /> 
    : <MailingPortal />;
};
```

### Test Cases

| ID | Title | Type | Priority |
|----|-------|------|----------|
| TC-001 | Portal cards render correctly | Unit | High |
| TC-002 | Analytics portal click navigates | E2E | Critical |
| TC-003 | Mailing portal click navigates | E2E | Critical |
| TC-004 | Portal switcher in header works | E2E | High |
| TC-005 | Last selection persists on refresh | E2E | High |
| TC-006 | Auth state preserved on switch | Integration | Critical |

### Acceptance Criteria

- [ ] Two portal cards displayed on selection page
- [ ] Clicking Analytics opens analytics dashboard
- [ ] Clicking Mailing opens mailing dashboard (empty state)
- [ ] Portal switcher visible in header
- [ ] Portal preference saved to localStorage
- [ ] Page refresh returns to last selected portal

---

## C003: List Management

### Overview
```yaml
component_id: C003
name: "List Management"
priority: P1
estimated_days: 5
dependencies: [C002]
```

### Business Requirements

**BR-001:** Users shall be able to create and manage mailing lists  
**BR-002:** Lists shall support custom fields for subscriber data  
**BR-003:** Users shall be able to configure opt-in/opt-out behavior  
**BR-004:** Users shall see list statistics (subscriber counts by status)  

### Functional Requirements

**FR-001:** List CRUD Operations
- Create list with name, description, settings
- View list details and statistics
- Update list settings
- Delete list (with confirmation if has subscribers)
- Copy list (with or without subscribers)

**FR-002:** List Settings
- Opt-in type: Single, Double
- Opt-out type: Single, Double
- Welcome email: Enable/Disable
- Subscriber approval required: Enable/Disable
- Visibility: Private, Public

**FR-003:** Custom Fields
- Field types: Text, Textarea, Dropdown, Checkbox, Checkbox List, Date, Number, Phone, URL, Rating
- Required/Optional setting
- Default value
- Drag-and-drop reordering
- Field validation rules

**FR-004:** List Statistics
- Total subscribers
- Confirmed subscribers
- Unconfirmed subscribers
- Unsubscribed count
- Blacklisted count
- 30-day growth chart

### Database Schema

```sql
CREATE TABLE mailing_lists (
    id BIGSERIAL PRIMARY KEY,
    uid VARCHAR(36) UNIQUE NOT NULL DEFAULT gen_random_uuid(),
    customer_id BIGINT NOT NULL,
    name VARCHAR(255) NOT NULL,
    display_name VARCHAR(255),
    description TEXT,
    visibility VARCHAR(20) DEFAULT 'private' CHECK (visibility IN ('private', 'public')),
    opt_in VARCHAR(20) DEFAULT 'double' CHECK (opt_in IN ('single', 'double')),
    opt_out VARCHAR(20) DEFAULT 'single' CHECK (opt_out IN ('single', 'double')),
    welcome_email BOOLEAN DEFAULT false,
    subscriber_require_approval BOOLEAN DEFAULT false,
    status VARCHAR(20) DEFAULT 'active' CHECK (status IN ('active', 'archived', 'pending-delete')),
    subscriber_count INTEGER DEFAULT 0,
    confirmed_count INTEGER DEFAULT 0,
    unconfirmed_count INTEGER DEFAULT 0,
    unsubscribed_count INTEGER DEFAULT 0,
    blacklisted_count INTEGER DEFAULT 0,
    meta_data JSONB,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(customer_id, name)
);

CREATE TABLE mailing_list_fields (
    id BIGSERIAL PRIMARY KEY,
    list_id BIGINT REFERENCES mailing_lists(id) ON DELETE CASCADE,
    label VARCHAR(255) NOT NULL,
    tag VARCHAR(50) NOT NULL,
    field_type VARCHAR(50) NOT NULL CHECK (field_type IN (
        'text', 'textarea', 'dropdown', 'checkbox', 'checkboxlist',
        'date', 'datetime', 'number', 'phone', 'url', 'rating', 'country', 'state'
    )),
    default_value TEXT,
    help_text VARCHAR(500),
    required BOOLEAN DEFAULT false,
    visible BOOLEAN DEFAULT true,
    sort_order INTEGER DEFAULT 0,
    validation_rules JSONB,
    meta_data JSONB,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(list_id, tag)
);

CREATE TABLE mailing_list_field_options (
    id BIGSERIAL PRIMARY KEY,
    field_id BIGINT REFERENCES mailing_list_fields(id) ON DELETE CASCADE,
    name VARCHAR(255) NOT NULL,
    value VARCHAR(255) NOT NULL,
    sort_order INTEGER DEFAULT 0
);

CREATE TABLE mailing_list_company (
    id BIGSERIAL PRIMARY KEY,
    list_id BIGINT UNIQUE REFERENCES mailing_lists(id) ON DELETE CASCADE,
    name VARCHAR(255),
    address_1 VARCHAR(255),
    address_2 VARCHAR(255),
    city VARCHAR(100),
    state VARCHAR(100),
    zip VARCHAR(20),
    country_id INTEGER,
    phone VARCHAR(50),
    website VARCHAR(255)
);

CREATE INDEX idx_mailing_lists_customer ON mailing_lists(customer_id);
CREATE INDEX idx_mailing_lists_status ON mailing_lists(status);
CREATE INDEX idx_mailing_list_fields_list ON mailing_list_fields(list_id);
```

### API Specification

```yaml
paths:
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
            maximum: 100
        - name: status
          in: query
          schema:
            type: string
            enum: [active, archived]
        - name: search
          in: query
          schema:
            type: string
      responses:
        200:
          content:
            application/json:
              schema:
                type: object
                properties:
                  data:
                    type: array
                    items:
                      $ref: '#/components/schemas/List'
                  pagination:
                    $ref: '#/components/schemas/Pagination'
    
    post:
      summary: Create a new list
      requestBody:
        content:
          application/json:
            schema:
              $ref: '#/components/schemas/CreateListRequest'
      responses:
        201:
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/List'

  /api/mailing/lists/{uid}:
    get:
      summary: Get list by UID
      responses:
        200:
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/ListDetail'
    
    put:
      summary: Update list
      requestBody:
        content:
          application/json:
            schema:
              $ref: '#/components/schemas/UpdateListRequest'
      responses:
        200:
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/List'
    
    delete:
      summary: Delete list
      responses:
        204:
          description: List deleted

  /api/mailing/lists/{uid}/stats:
    get:
      summary: Get list statistics
      responses:
        200:
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/ListStats'

  /api/mailing/lists/{uid}/copy:
    post:
      summary: Copy list
      requestBody:
        content:
          application/json:
            schema:
              type: object
              properties:
                name:
                  type: string
                include_subscribers:
                  type: boolean
                  default: false
      responses:
        201:
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/List'

  /api/mailing/lists/{uid}/fields:
    get:
      summary: Get list fields
      responses:
        200:
          content:
            application/json:
              schema:
                type: array
                items:
                  $ref: '#/components/schemas/ListField'
    
    post:
      summary: Add custom field
      requestBody:
        content:
          application/json:
            schema:
              $ref: '#/components/schemas/CreateFieldRequest'
      responses:
        201:
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/ListField'

  /api/mailing/lists/{uid}/fields/{fieldId}:
    put:
      summary: Update field
    delete:
      summary: Delete field

  /api/mailing/lists/{uid}/fields/reorder:
    put:
      summary: Reorder fields
      requestBody:
        content:
          application/json:
            schema:
              type: object
              properties:
                field_ids:
                  type: array
                  items:
                    type: integer

components:
  schemas:
    List:
      type: object
      properties:
        id:
          type: integer
        uid:
          type: string
        name:
          type: string
        display_name:
          type: string
        description:
          type: string
        opt_in:
          type: string
          enum: [single, double]
        opt_out:
          type: string
          enum: [single, double]
        welcome_email:
          type: boolean
        status:
          type: string
        subscriber_count:
          type: integer
        created_at:
          type: string
          format: date-time

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
        subscriber_require_approval:
          type: boolean
          default: false

    ListField:
      type: object
      properties:
        id:
          type: integer
        label:
          type: string
        tag:
          type: string
        field_type:
          type: string
        default_value:
          type: string
        required:
          type: boolean
        sort_order:
          type: integer
        options:
          type: array
          items:
            type: object
            properties:
              name:
                type: string
              value:
                type: string

    ListStats:
      type: object
      properties:
        total:
          type: integer
        confirmed:
          type: integer
        unconfirmed:
          type: integer
        unsubscribed:
          type: integer
        blacklisted:
          type: integer
        growth_30d:
          type: array
          items:
            type: object
            properties:
              date:
                type: string
              count:
                type: integer
```

### Backend Implementation

#### Project Structure
```
internal/mailing/lists/
├── service.go          # ListService interface and implementation
├── repository.go       # ListRepository for database operations
├── handlers.go         # HTTP handlers
├── types.go            # Domain types and DTOs
├── validation.go       # Input validation
├── service_test.go     # Service unit tests
├── handlers_test.go    # Handler tests
└── repository_test.go  # Repository tests
```

#### Service Interface
```go
type ListService interface {
    Create(ctx context.Context, customerID int64, req *CreateListRequest) (*List, error)
    GetByUID(ctx context.Context, uid string) (*List, error)
    GetByCustomer(ctx context.Context, customerID int64, opts *ListOptions) (*ListPage, error)
    Update(ctx context.Context, uid string, req *UpdateListRequest) (*List, error)
    Delete(ctx context.Context, uid string) error
    Copy(ctx context.Context, uid string, req *CopyListRequest) (*List, error)
    GetStats(ctx context.Context, uid string) (*ListStats, error)
    
    // Fields
    GetFields(ctx context.Context, listUID string) ([]*ListField, error)
    AddField(ctx context.Context, listUID string, req *CreateFieldRequest) (*ListField, error)
    UpdateField(ctx context.Context, listUID string, fieldID int64, req *UpdateFieldRequest) (*ListField, error)
    DeleteField(ctx context.Context, listUID string, fieldID int64) error
    ReorderFields(ctx context.Context, listUID string, fieldIDs []int64) error
}
```

### Frontend Components

```
web/src/components/mailing/lists/
├── ListsDashboard.tsx      # Main lists view with table
├── ListForm.tsx            # Create/Edit list form
├── ListDetails.tsx         # Single list detail view
├── ListStats.tsx           # Statistics display
├── ListFields.tsx          # Custom fields manager
├── FieldForm.tsx           # Add/Edit field form
├── types.ts                # TypeScript interfaces
├── hooks.ts                # useLists, useList, useListFields
├── index.ts                # Barrel exports
└── *.test.tsx              # Component tests
```

### Test Cases

| ID | Title | Type | Priority |
|----|-------|------|----------|
| TC-010 | Create list with valid data | Integration | Critical |
| TC-011 | Create list validation errors | Unit | High |
| TC-012 | Get lists with pagination | Integration | High |
| TC-013 | Get list by UID | Integration | High |
| TC-014 | Update list settings | Integration | High |
| TC-015 | Delete list without subscribers | Integration | High |
| TC-016 | Delete list with subscribers shows confirm | E2E | High |
| TC-017 | Copy list without subscribers | Integration | Medium |
| TC-018 | Copy list with subscribers | Integration | Medium |
| TC-019 | Add text custom field | Integration | High |
| TC-020 | Add dropdown field with options | Integration | High |
| TC-021 | Update field | Integration | Medium |
| TC-022 | Delete field | Integration | Medium |
| TC-023 | Reorder fields | Integration | Medium |
| TC-024 | View list statistics | Integration | High |
| TC-025 | Full list creation E2E flow | E2E | Critical |

### Domain Expert Validation Points

**Email Marketing Expert:**
- [ ] Can create lists with intuitive workflow (like Mailchimp)
- [ ] Custom fields cover all common use cases
- [ ] Opt-in/opt-out options are clearly explained
- [ ] Statistics are meaningful and actionable
- [ ] List management is efficient for large numbers of lists

**Deliverability Specialist:**
- [ ] Double opt-in is properly supported
- [ ] Unsubscribe handling follows best practices
- [ ] Company information supports CAN-SPAM compliance

---

## C007: Campaign Builder

### Overview
```yaml
component_id: C007
name: "Campaign Builder"
priority: P3
estimated_days: 7
dependencies: [C003, C005, C006]
```

### Business Requirements

**BR-001:** Users shall be able to create email campaigns  
**BR-002:** Campaign creation shall follow a wizard-style multi-step flow  
**BR-003:** Users shall be able to preview campaigns before sending  
**BR-004:** Users shall be able to schedule campaigns for future delivery  
**BR-005:** Users shall be able to send test emails  

### Functional Requirements

**FR-001:** Campaign Types
- Regular campaigns (one-time send)
- Autoresponder campaigns (triggered)

**FR-002:** Campaign Wizard Steps
1. **Name & List**: Campaign name, select list, optional segment
2. **Setup**: From name, from email, reply-to, subject, preheader
3. **Template**: Select or create email template
4. **Confirm**: Review settings, schedule, send test

**FR-003:** Campaign Settings
- Subject line with personalization tags
- Preheader text
- From name (with personalization)
- From email (validated sending domain)
- Reply-to email
- Tracking options (opens, clicks)

**FR-004:** Scheduling
- Send immediately
- Schedule for specific date/time
- Timezone handling
- Timewarp (send at subscriber's local time)

**FR-005:** Campaign Actions
- Save as draft
- Send test email
- Schedule campaign
- Pause/Resume campaign
- Cancel campaign

### Database Schema

```sql
CREATE TABLE mailing_campaigns (
    id BIGSERIAL PRIMARY KEY,
    uid VARCHAR(36) UNIQUE NOT NULL DEFAULT gen_random_uuid(),
    customer_id BIGINT NOT NULL,
    list_id BIGINT REFERENCES mailing_lists(id),
    segment_id BIGINT REFERENCES mailing_segments(id),
    template_id BIGINT REFERENCES mailing_templates(id),
    type VARCHAR(20) DEFAULT 'regular' CHECK (type IN ('regular', 'autoresponder')),
    name VARCHAR(255) NOT NULL,
    subject VARCHAR(500) NOT NULL,
    subject_encoded VARCHAR(1000),
    from_name VARCHAR(255) NOT NULL,
    from_email VARCHAR(255) NOT NULL,
    reply_to VARCHAR(255),
    preheader VARCHAR(255),
    
    -- Sending settings
    send_at TIMESTAMP,
    send_between_start TIME,
    send_between_end TIME,
    timewarp_enabled BOOLEAN DEFAULT false,
    timewarp_hour SMALLINT,
    timewarp_minute SMALLINT,
    
    -- Tracking settings
    track_opens BOOLEAN DEFAULT true,
    track_clicks BOOLEAN DEFAULT true,
    
    -- Status
    status VARCHAR(30) DEFAULT 'draft' CHECK (status IN (
        'draft', 'pending-sending', 'sending', 'sent', 
        'paused', 'pending-delete', 'blocked', 'pending-approve'
    )),
    started_at TIMESTAMP,
    finished_at TIMESTAMP,
    
    -- Counters (updated by workers)
    total_recipients INTEGER DEFAULT 0,
    processed_count INTEGER DEFAULT 0,
    sent_count INTEGER DEFAULT 0,
    delivered_count INTEGER DEFAULT 0,
    bounced_count INTEGER DEFAULT 0,
    opened_count INTEGER DEFAULT 0,
    unique_opens_count INTEGER DEFAULT 0,
    clicked_count INTEGER DEFAULT 0,
    unique_clicks_count INTEGER DEFAULT 0,
    unsubscribed_count INTEGER DEFAULT 0,
    complained_count INTEGER DEFAULT 0,
    
    -- Metadata
    delivery_servers JSONB,
    priority INTEGER DEFAULT 5,
    meta_data JSONB,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE mailing_campaign_options (
    campaign_id BIGINT PRIMARY KEY REFERENCES mailing_campaigns(id) ON DELETE CASCADE,
    
    -- Tracking
    smart_open_tracking BOOLEAN DEFAULT false,
    smart_click_tracking BOOLEAN DEFAULT false,
    
    -- Content
    json_feed VARCHAR(500),
    xml_feed VARCHAR(500),
    embed_images BOOLEAN DEFAULT false,
    plain_text_email BOOLEAN DEFAULT true,
    
    -- Autoresponder
    autoresponder_event VARCHAR(50),
    autoresponder_time_value INTEGER,
    autoresponder_time_unit VARCHAR(20),
    autoresponder_include_imported BOOLEAN DEFAULT false,
    
    -- Recurring
    cronjob_enabled BOOLEAN DEFAULT false,
    cronjob_expression VARCHAR(100),
    cronjob_max_runs INTEGER DEFAULT -1,
    cronjob_runs_counter INTEGER DEFAULT 0,
    
    -- Sharing
    share_reports_enabled BOOLEAN DEFAULT false,
    share_reports_password VARCHAR(255),
    
    -- Limits
    max_send_count INTEGER,
    giveup_count INTEGER DEFAULT 3
);

CREATE TABLE mailing_campaign_delivery_servers (
    id BIGSERIAL PRIMARY KEY,
    campaign_id BIGINT REFERENCES mailing_campaigns(id) ON DELETE CASCADE,
    server_id BIGINT REFERENCES mailing_delivery_servers(id),
    UNIQUE(campaign_id, server_id)
);

CREATE INDEX idx_campaigns_customer ON mailing_campaigns(customer_id);
CREATE INDEX idx_campaigns_list ON mailing_campaigns(list_id);
CREATE INDEX idx_campaigns_status ON mailing_campaigns(status);
CREATE INDEX idx_campaigns_send_at ON mailing_campaigns(send_at) WHERE status = 'pending-sending';
```

### Campaign Wizard UI Flow

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                           CAMPAIGN WIZARD                                    │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│  Step 1          Step 2          Step 3          Step 4                     │
│  ┌──────┐       ┌──────┐       ┌──────┐       ┌──────┐                     │
│  │ Name │ ───▶ │Setup │ ───▶ │Template│ ───▶ │Confirm│                     │
│  │& List│       │      │       │       │       │      │                     │
│  └──────┘       └──────┘       └──────┘       └──────┘                     │
│    [●]            [○]            [○]            [○]                         │
│                                                                              │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│  STEP 1: NAME & LIST                                                        │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │ Campaign Name *                                                      │   │
│  │ ┌───────────────────────────────────────────────────────────────┐   │   │
│  │ │ February Newsletter                                            │   │   │
│  │ └───────────────────────────────────────────────────────────────┘   │   │
│  │                                                                      │   │
│  │ Select List *                                                        │   │
│  │ ┌───────────────────────────────────────────────────────────────┐   │   │
│  │ │ Newsletter Subscribers (45,230 subscribers)              ▼    │   │   │
│  │ └───────────────────────────────────────────────────────────────┘   │   │
│  │                                                                      │   │
│  │ ☐ Send to segment only                                              │   │
│  │   ┌───────────────────────────────────────────────────────────────┐│   │
│  │   │ Select segment...                                          ▼  ││   │
│  │   └───────────────────────────────────────────────────────────────┘│   │
│  └─────────────────────────────────────────────────────────────────────┘   │
│                                                                              │
│                                              [Cancel]  [Save Draft]  [Next] │
└─────────────────────────────────────────────────────────────────────────────┘
```

### API Specification

```yaml
paths:
  /api/mailing/campaigns:
    get:
      summary: Get campaigns
      parameters:
        - name: status
          in: query
          schema:
            type: string
            enum: [draft, pending-sending, sending, sent, paused]
        - name: type
          in: query
          schema:
            type: string
            enum: [regular, autoresponder]
    post:
      summary: Create campaign
      requestBody:
        content:
          application/json:
            schema:
              $ref: '#/components/schemas/CreateCampaignRequest'

  /api/mailing/campaigns/{uid}:
    get:
      summary: Get campaign details
    put:
      summary: Update campaign
    delete:
      summary: Delete campaign

  /api/mailing/campaigns/{uid}/send:
    post:
      summary: Start sending campaign
      requestBody:
        content:
          application/json:
            schema:
              type: object
              properties:
                schedule_at:
                  type: string
                  format: date-time
                  description: "Optional - schedule for later"

  /api/mailing/campaigns/{uid}/pause:
    post:
      summary: Pause sending

  /api/mailing/campaigns/{uid}/resume:
    post:
      summary: Resume sending

  /api/mailing/campaigns/{uid}/test:
    post:
      summary: Send test email
      requestBody:
        content:
          application/json:
            schema:
              type: object
              required:
                - email
              properties:
                email:
                  type: string
                  format: email

  /api/mailing/campaigns/{uid}/stats:
    get:
      summary: Get campaign statistics

  /api/mailing/campaigns/{uid}/preview:
    get:
      summary: Get campaign preview HTML
      parameters:
        - name: subscriber_uid
          in: query
          description: "Optional - preview with specific subscriber data"
```

### Test Cases

| ID | Title | Type | Priority |
|----|-------|------|----------|
| TC-070 | Create campaign step 1 (name & list) | E2E | Critical |
| TC-071 | Create campaign step 2 (setup) | E2E | Critical |
| TC-072 | Create campaign step 3 (template) | E2E | Critical |
| TC-073 | Create campaign step 4 (confirm) | E2E | Critical |
| TC-074 | Save campaign as draft | Integration | High |
| TC-075 | Send test email | Integration | Critical |
| TC-076 | Schedule campaign for future | Integration | High |
| TC-077 | Send campaign immediately | Integration | Critical |
| TC-078 | Pause sending campaign | Integration | High |
| TC-079 | Resume paused campaign | Integration | High |
| TC-080 | Subject line personalization | Integration | High |
| TC-081 | Preview with subscriber data | Integration | Medium |
| TC-082 | Campaign with segment | Integration | High |
| TC-083 | Timewarp scheduling | Integration | Medium |

---

## C009: Sending Engine

### Overview
```yaml
component_id: C009
name: "Sending Engine"
priority: P4
estimated_days: 10
dependencies: [C007, C008]
```

### Business Requirements

**BR-001:** System shall send emails at scale (8M+ per day)  
**BR-002:** System shall route emails through configured ESPs  
**BR-003:** System shall respect ESP quotas and throttling limits  
**BR-004:** System shall handle failures with retry logic  
**BR-005:** System shall support multiple sending domains  

### Architecture

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                           SENDING ENGINE                                     │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│  ┌─────────────┐                                                            │
│  │  Scheduler  │  (Checks for scheduled campaigns every minute)             │
│  └──────┬──────┘                                                            │
│         │                                                                    │
│         ▼                                                                    │
│  ┌─────────────┐                                                            │
│  │  Campaign   │  (Loads campaign, segments subscribers)                    │
│  │  Processor  │                                                            │
│  └──────┬──────┘                                                            │
│         │                                                                    │
│         ▼                                                                    │
│  ┌─────────────┐                                                            │
│  │  Batch      │  (Creates batches of 100 subscribers)                      │
│  │  Creator    │                                                            │
│  └──────┬──────┘                                                            │
│         │                                                                    │
│         ▼                                                                    │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │                         REDIS QUEUE                                  │   │
│  │  ┌─────────┐  ┌─────────┐  ┌─────────┐  ┌─────────┐  ┌─────────┐   │   │
│  │  │ :high   │  │ :normal │  │ :low    │  │:scheduled│  │  :dlq   │   │   │
│  │  └─────────┘  └─────────┘  └─────────┘  └─────────┘  └─────────┘   │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
│         │                                                                    │
│         ▼                                                                    │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │                       SEND WORKERS (x20)                             │   │
│  │                                                                      │   │
│  │  ┌───────────────────────────────────────────────────────────────┐  │   │
│  │  │ 1. Dequeue job                                                │  │   │
│  │  │ 2. Check throttle (token bucket)                              │  │   │
│  │  │ 3. Render template with subscriber data                       │  │   │
│  │  │ 4. Select delivery server (load balance + quota check)        │  │   │
│  │  │ 5. Send via ESP (SparkPost / SES)                            │  │   │
│  │  │ 6. Log result (success/failure)                               │  │   │
│  │  │ 7. Update campaign counters                                   │  │   │
│  │  └───────────────────────────────────────────────────────────────┘  │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
│                                                                              │
└─────────────────────────────────────────────────────────────────────────────┘
```

### Queue Job Structure

```go
type SendJob struct {
    ID            string    `json:"id"`
    CampaignUID   string    `json:"campaign_uid"`
    SubscriberUID string    `json:"subscriber_uid"`
    ListUID       string    `json:"list_uid"`
    
    // Pre-computed for performance
    ToEmail       string    `json:"to_email"`
    ToName        string    `json:"to_name"`
    
    // Routing
    ServerUID     string    `json:"server_uid,omitempty"`
    Domain        string    `json:"domain"`
    
    // Scheduling
    Priority      int       `json:"priority"`      // 1=high, 5=low
    ScheduledAt   time.Time `json:"scheduled_at,omitempty"`
    
    // Retry tracking
    Attempts      int       `json:"attempts"`
    MaxAttempts   int       `json:"max_attempts"`
    LastError     string    `json:"last_error,omitempty"`
    
    CreatedAt     time.Time `json:"created_at"`
}
```

### Throttling Configuration

```yaml
throttling:
  # Server-level (token bucket)
  server_defaults:
    refill_rate: 100        # tokens per second
    bucket_size: 1000       # max burst
  
  # Domain-level limits (per hour)
  domain_limits:
    "gmail.com": 500
    "googlemail.com": 500
    "yahoo.com": 400
    "yahoo.co.uk": 400
    "aol.com": 400
    "outlook.com": 300
    "hotmail.com": 300
    "live.com": 300
    "icloud.com": 300
    "me.com": 300
    "comcast.net": 200
    "att.net": 400
    "sbcglobal.net": 400
    "bellsouth.net": 400
    "_default": 100
  
  # Adaptive adjustments
  adaptive:
    enabled: true
    bounce_threshold: 0.05    # Reduce rate if bounce > 5%
    complaint_threshold: 0.001 # Reduce rate if complaint > 0.1%
    reduction_factor: 0.5     # Reduce to 50% on threshold breach
```

### ESP Integration

#### SparkPost
```go
type SparkPostSender struct {
    client   *sparkpost.Client
    throttle *TokenBucket
}

func (s *SparkPostSender) Send(ctx context.Context, job *SendJob, content *EmailContent) (*SendResult, error) {
    tx := &sparkpost.Transmission{
        Recipients: []sparkpost.Recipient{{
            Address: sparkpost.Address{
                Email: job.ToEmail,
                Name:  job.ToName,
            },
        }},
        Content: sparkpost.Content{
            From:    content.From,
            Subject: content.Subject,
            HTML:    content.HTML,
            Text:    content.Text,
        },
        Options: &sparkpost.TxOptions{
            OpenTracking:  content.TrackOpens,
            ClickTracking: content.TrackClicks,
        },
        Metadata: map[string]interface{}{
            "campaign_uid":   job.CampaignUID,
            "subscriber_uid": job.SubscriberUID,
        },
    }
    
    id, _, err := s.client.Send(tx)
    if err != nil {
        return &SendResult{Success: false, Error: err.Error()}, err
    }
    
    return &SendResult{Success: true, MessageID: id}, nil
}
```

#### AWS SES
```go
type SESSender struct {
    client   *ses.Client
    throttle *TokenBucket
}

func (s *SESSender) Send(ctx context.Context, job *SendJob, content *EmailContent) (*SendResult, error) {
    input := &ses.SendEmailInput{
        Destination: &types.Destination{
            ToAddresses: []string{job.ToEmail},
        },
        Message: &types.Message{
            Subject: &types.Content{
                Data: aws.String(content.Subject),
            },
            Body: &types.Body{
                Html: &types.Content{
                    Data: aws.String(content.HTML),
                },
                Text: &types.Content{
                    Data: aws.String(content.Text),
                },
            },
        },
        Source: aws.String(content.From),
        Tags: []types.MessageTag{
            {Name: aws.String("campaign"), Value: aws.String(job.CampaignUID)},
            {Name: aws.String("subscriber"), Value: aws.String(job.SubscriberUID)},
        },
    }
    
    result, err := s.client.SendEmail(ctx, input)
    if err != nil {
        return &SendResult{Success: false, Error: err.Error()}, err
    }
    
    return &SendResult{Success: true, MessageID: *result.MessageId}, nil
}
```

### Test Cases

| ID | Title | Type | Priority |
|----|-------|------|----------|
| TC-090 | Enqueue campaign for sending | Integration | Critical |
| TC-091 | Worker processes job successfully | Integration | Critical |
| TC-092 | Throttle prevents over-sending | Unit | Critical |
| TC-093 | Server rotation on quota | Integration | High |
| TC-094 | Retry on temporary failure | Integration | High |
| TC-095 | DLQ on permanent failure | Integration | High |
| TC-096 | Campaign progress updates | Integration | High |
| TC-097 | Pause campaign stops workers | Integration | High |
| TC-098 | Send via SparkPost | Integration | Critical |
| TC-099 | Send via AWS SES | Integration | Critical |
| TC-100 | Domain-level throttling | Unit | High |
| TC-101 | Adaptive throttling on bounce | Unit | Medium |
| TC-102 | Load test 1000 emails/sec | Performance | Critical |

---

## Quality Metrics Summary

### Per-Component Targets

| Metric | Target | Validation |
|--------|--------|------------|
| Code Coverage | ≥ 90% | `go test -cover` / `vitest --coverage` |
| Unit Tests | 100% pass | CI pipeline |
| Integration Tests | 100% pass | Docker compose test |
| E2E Tests | 100% pass | Playwright |
| API Latency (p99) | < 200ms | k6 load test |
| Security Scan | 0 critical/high | Snyk |
| Domain Expert Approval | Yes | Review checklist |
| Enterprise Ready | Yes | Checklist complete |

### Overall Platform Targets

| Metric | Target |
|--------|--------|
| Daily Send Capacity | 8,000,000+ |
| Tracking Throughput | 1,000 events/sec |
| System Uptime | 99.9% |
| Delivery Rate | > 98% |
| API Error Rate | < 0.1% |

---

**Document End**

*This document provides detailed specifications for each component to be built.*
