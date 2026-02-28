# Security Architecture

**Version:** 1.0.0  
**Status:** COMPLETE  
**Last Updated:** February 1, 2026  

---

## Table of Contents

1. [Security Overview](#1-security-overview)
2. [Authentication](#2-authentication)
3. [Authorization](#3-authorization)
4. [Data Protection](#4-data-protection)
5. [API Security](#5-api-security)
6. [Infrastructure Security](#6-infrastructure-security)
7. [Compliance](#7-compliance)
8. [Incident Response](#8-incident-response)

---

## 1. Security Overview

### 1.1 Security Principles

1. **Defense in Depth** - Multiple layers of security controls
2. **Least Privilege** - Minimal access required for each role
3. **Zero Trust** - Verify every request, trust nothing
4. **Secure by Default** - Security enabled out of the box
5. **Fail Secure** - System fails to a secure state

### 1.2 Security Architecture Diagram

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                           SECURITY ARCHITECTURE                                  │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                  │
│  INTERNET                                                                        │
│      │                                                                           │
│      ▼                                                                           │
│  ┌────────────────────────────────────────────────────────────────────────┐     │
│  │                    WAF (AWS WAF / CloudFlare)                          │     │
│  │  • DDoS Protection  • SQL Injection  • XSS  • Rate Limiting           │     │
│  └────────────────────────────────────────────────────────────────────────┘     │
│      │                                                                           │
│      ▼                                                                           │
│  ┌────────────────────────────────────────────────────────────────────────┐     │
│  │                    Load Balancer (ALB)                                 │     │
│  │  • TLS 1.3 Termination  • Certificate Management                      │     │
│  └────────────────────────────────────────────────────────────────────────┘     │
│      │                                                                           │
│      ▼                                                                           │
│  ┌────────────────────────────────────────────────────────────────────────┐     │
│  │                    API Gateway (Traefik)                               │     │
│  │  • Rate Limiting  • Authentication  • Request Validation              │     │
│  └────────────────────────────────────────────────────────────────────────┘     │
│      │                                                                           │
│      ├──────────────────┬──────────────────┬──────────────────┐                 │
│      ▼                  ▼                  ▼                  ▼                 │
│  ┌──────────┐     ┌──────────┐     ┌──────────┐     ┌──────────┐               │
│  │ Mailing  │     │ Analytics │     │ Tracking │     │ Webhook  │               │
│  │   API    │     │   API    │     │ Service  │     │ Handler  │               │
│  └──────────┘     └──────────┘     └──────────┘     └──────────┘               │
│      │                  │                  │                  │                 │
│  ════╪══════════════════╪══════════════════╪══════════════════╪════════════     │
│      │                  │                  │                  │                 │
│      ▼                  ▼                  ▼                  ▼                 │
│  ┌────────────────────────────────────────────────────────────────────────┐     │
│  │                         VPC / Private Subnet                           │     │
│  │                                                                        │     │
│  │  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐                │     │
│  │  │  PostgreSQL  │  │    Redis     │  │   DynamoDB   │                │     │
│  │  │  (Encrypted) │  │  (Auth TLS)  │  │  (Encrypted) │                │     │
│  │  └──────────────┘  └──────────────┘  └──────────────┘                │     │
│  │                                                                        │     │
│  │  ┌──────────────┐                                                     │     │
│  │  │ Secrets Mgr  │ (AWS Secrets Manager)                              │     │
│  │  └──────────────┘                                                     │     │
│  └────────────────────────────────────────────────────────────────────────┘     │
│                                                                                  │
└─────────────────────────────────────────────────────────────────────────────────┘
```

---

## 2. Authentication

### 2.1 Google OAuth 2.0 Flow

```
┌──────────┐     ┌──────────┐     ┌──────────┐     ┌──────────┐
│  Browser │     │   API    │     │  Google  │     │  Redis   │
└────┬─────┘     └────┬─────┘     └────┬─────┘     └────┬─────┘
     │                │                │                │
     │ 1. Click Login │                │                │
     │───────────────▶│                │                │
     │                │                │                │
     │ 2. Generate state, store       │                │
     │                │────────────────────────────────▶│
     │                │                │                │
     │ 3. Redirect to Google           │                │
     │◀───────────────│                │                │
     │                │                │                │
     │ 4. User authenticates           │                │
     │────────────────────────────────▶│                │
     │                │                │                │
     │ 5. Redirect with code           │                │
     │◀────────────────────────────────│                │
     │                │                │                │
     │ 6. Exchange code                │                │
     │───────────────▶│────────────────▶                │
     │                │◀────────────────│                │
     │                │                │                │
     │ 7. Verify state                 │                │
     │                │────────────────────────────────▶│
     │                │◀───────────────────────────────│
     │                │                │                │
     │ 8. Create session               │                │
     │                │────────────────────────────────▶│
     │                │                │                │
     │ 9. Return JWT + Set Cookie      │                │
     │◀───────────────│                │                │
     │                │                │                │
```

### 2.2 Session Management

```yaml
session:
  storage: redis
  key_prefix: "session:"
  
  jwt:
    algorithm: RS256
    issuer: "ignite.mailing.platform"
    audience: "ignite-api"
    expiry: 1h
    refresh_expiry: 24h
    
  cookie:
    name: "ignite_session"
    http_only: true
    secure: true
    same_site: strict
    path: "/"
    max_age: 86400  # 24 hours
    
  token_structure:
    header:
      alg: RS256
      typ: JWT
    payload:
      sub: user_uid
      org: organization_uid
      role: user_role
      iat: issued_at
      exp: expiry
      jti: token_id
```

### 2.3 API Key Authentication

```yaml
api_key:
  format: "ignite_{random_32_bytes_base64}"
  prefix_length: 8  # For identification
  
  storage:
    table: api_keys
    hash_algorithm: SHA-256
    
  rate_limits:
    default: 1000/hour
    premium: 10000/hour
    
  permissions:
    - all
    - lists:read
    - lists:write
    - campaigns:read
    - campaigns:write
    - campaigns:send
    - subscribers:read
    - subscribers:write
    - analytics:read
```

### 2.4 Authentication Middleware

```go
// internal/auth/middleware.go

func AuthMiddleware(sessionStore SessionStore) func(http.Handler) http.Handler {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            // 1. Extract token from cookie or Authorization header
            token := extractToken(r)
            if token == "" {
                http.Error(w, "Unauthorized", http.StatusUnauthorized)
                return
            }
            
            // 2. Verify JWT signature and expiry
            claims, err := verifyJWT(token)
            if err != nil {
                http.Error(w, "Invalid token", http.StatusUnauthorized)
                return
            }
            
            // 3. Check session exists in Redis
            session, err := sessionStore.Get(claims.JTI)
            if err != nil || session == nil {
                http.Error(w, "Session expired", http.StatusUnauthorized)
                return
            }
            
            // 4. Add user context
            ctx := context.WithValue(r.Context(), UserContextKey, &UserContext{
                UserID:         claims.Sub,
                OrganizationID: claims.Org,
                Role:           claims.Role,
            })
            
            next.ServeHTTP(w, r.WithContext(ctx))
        })
    }
}

func APIKeyMiddleware(apiKeyStore APIKeyStore) func(http.Handler) http.Handler {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            // 1. Extract API key from header
            apiKey := r.Header.Get("X-API-Key")
            if apiKey == "" {
                http.Error(w, "API key required", http.StatusUnauthorized)
                return
            }
            
            // 2. Hash and lookup
            prefix := apiKey[:8]
            hash := sha256.Sum256([]byte(apiKey))
            
            key, err := apiKeyStore.GetByPrefixAndHash(prefix, hex.EncodeToString(hash[:]))
            if err != nil || key == nil {
                http.Error(w, "Invalid API key", http.StatusUnauthorized)
                return
            }
            
            // 3. Check expiry and status
            if key.Status != "active" || (key.ExpiresAt != nil && key.ExpiresAt.Before(time.Now())) {
                http.Error(w, "API key expired or revoked", http.StatusUnauthorized)
                return
            }
            
            // 4. Update last used
            apiKeyStore.UpdateLastUsed(key.ID)
            
            // 5. Add context
            ctx := context.WithValue(r.Context(), APIKeyContextKey, &APIKeyContext{
                KeyID:          key.ID,
                OrganizationID: key.OrganizationID,
                Permissions:    key.Permissions,
            })
            
            next.ServeHTTP(w, r.WithContext(ctx))
        })
    }
}
```

---

## 3. Authorization

### 3.1 Role-Based Access Control (RBAC)

```yaml
roles:
  owner:
    description: "Organization owner with full access"
    permissions:
      - "*"  # All permissions
      
  admin:
    description: "Administrator with most access"
    permissions:
      - "lists:*"
      - "subscribers:*"
      - "campaigns:*"
      - "templates:*"
      - "segments:*"
      - "servers:*"
      - "analytics:*"
      - "settings:read"
      - "users:read"
      - "users:invite"
      
  user:
    description: "Regular user"
    permissions:
      - "lists:read"
      - "lists:write"
      - "subscribers:read"
      - "subscribers:write"
      - "campaigns:read"
      - "campaigns:write"
      - "campaigns:send"
      - "templates:read"
      - "templates:write"
      - "segments:read"
      - "segments:write"
      - "servers:read"
      - "analytics:read"
      
  viewer:
    description: "Read-only access"
    permissions:
      - "lists:read"
      - "subscribers:read"
      - "campaigns:read"
      - "templates:read"
      - "segments:read"
      - "servers:read"
      - "analytics:read"
```

### 3.2 Permission Matrix

| Resource | Owner | Admin | User | Viewer |
|----------|-------|-------|------|--------|
| **Organization** |
| View settings | ✅ | ✅ | ✅ | ✅ |
| Edit settings | ✅ | ❌ | ❌ | ❌ |
| Manage users | ✅ | ✅ | ❌ | ❌ |
| Manage API keys | ✅ | ✅ | ❌ | ❌ |
| **Lists** |
| View | ✅ | ✅ | ✅ | ✅ |
| Create | ✅ | ✅ | ✅ | ❌ |
| Edit | ✅ | ✅ | ✅ | ❌ |
| Delete | ✅ | ✅ | ✅ | ❌ |
| **Subscribers** |
| View | ✅ | ✅ | ✅ | ✅ |
| Create | ✅ | ✅ | ✅ | ❌ |
| Edit | ✅ | ✅ | ✅ | ❌ |
| Delete | ✅ | ✅ | ✅ | ❌ |
| Import | ✅ | ✅ | ✅ | ❌ |
| Export | ✅ | ✅ | ✅ | ❌ |
| **Campaigns** |
| View | ✅ | ✅ | ✅ | ✅ |
| Create | ✅ | ✅ | ✅ | ❌ |
| Edit | ✅ | ✅ | ✅ | ❌ |
| Delete | ✅ | ✅ | ✅ | ❌ |
| Send | ✅ | ✅ | ✅ | ❌ |
| **Delivery Servers** |
| View | ✅ | ✅ | ✅ | ✅ |
| Create | ✅ | ✅ | ❌ | ❌ |
| Edit | ✅ | ✅ | ❌ | ❌ |
| Delete | ✅ | ✅ | ❌ | ❌ |

### 3.3 Multi-tenant Isolation

```go
// Every database query MUST include organization_id
type OrganizationScope struct {
    db *sql.DB
}

func (s *OrganizationScope) Query(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
    orgID := GetOrganizationID(ctx)
    if orgID == 0 {
        return nil, errors.New("organization context required")
    }
    
    // Inject organization filter
    scopedQuery := fmt.Sprintf("%s AND organization_id = $%d", query, len(args)+1)
    args = append(args, orgID)
    
    return s.db.QueryContext(ctx, scopedQuery, args...)
}
```

---

## 4. Data Protection

### 4.1 Encryption at Rest

```yaml
encryption_at_rest:
  postgresql:
    method: "AWS RDS Encryption"
    algorithm: "AES-256"
    key_management: "AWS KMS"
    key_id: "alias/ignite-rds-key"
    
  dynamodb:
    method: "AWS DynamoDB Encryption"
    algorithm: "AES-256"
    key_management: "AWS Owned CMK"
    
  s3:
    method: "Server-Side Encryption"
    algorithm: "AES-256"
    key_management: "AWS KMS"
    key_id: "alias/ignite-s3-key"
    
  redis:
    method: "AWS ElastiCache Encryption"
    algorithm: "AES-256"
    in_transit: true
```

### 4.2 Encryption in Transit

```yaml
encryption_in_transit:
  tls:
    minimum_version: "TLS 1.2"
    preferred_version: "TLS 1.3"
    cipher_suites:
      - TLS_AES_256_GCM_SHA384
      - TLS_CHACHA20_POLY1305_SHA256
      - TLS_AES_128_GCM_SHA256
    certificate_management: "AWS ACM"
    
  internal_communication:
    service_mesh: false  # Future consideration
    mtls: false  # Future consideration
    
  database_connections:
    postgresql: "require SSL"
    redis: "TLS enabled"
```

### 4.3 Sensitive Data Handling

```yaml
sensitive_data:
  # Fields that contain sensitive data
  pii_fields:
    - subscriber.email
    - subscriber.ip_address
    - user.email
    - user.name
    
  # Fields that must be encrypted
  encrypted_fields:
    - delivery_server.sparkpost_api_key
    - delivery_server.ses_access_key
    - delivery_server.ses_secret_key
    - delivery_server.smtp_password
    
  # Encryption implementation
  field_encryption:
    algorithm: "AES-256-GCM"
    key_derivation: "PBKDF2-SHA256"
    master_key_source: "AWS Secrets Manager"
    key_rotation: "90 days"
```

### 4.4 Secrets Management

```yaml
secrets_management:
  provider: "AWS Secrets Manager"
  
  secrets:
    - name: "ignite/prod/database"
      keys:
        - POSTGRES_HOST
        - POSTGRES_USER
        - POSTGRES_PASSWORD
        - POSTGRES_DB
        
    - name: "ignite/prod/redis"
      keys:
        - REDIS_URL
        - REDIS_PASSWORD
        
    - name: "ignite/prod/oauth"
      keys:
        - GOOGLE_CLIENT_ID
        - GOOGLE_CLIENT_SECRET
        
    - name: "ignite/prod/encryption"
      keys:
        - FIELD_ENCRYPTION_KEY
        - JWT_PRIVATE_KEY
        - JWT_PUBLIC_KEY
        
  rotation:
    database_credentials: "30 days"
    api_keys: "90 days"
    encryption_keys: "365 days"
```

---

## 5. API Security

### 5.1 Rate Limiting

```yaml
rate_limiting:
  global:
    requests_per_second: 10000
    burst_size: 20000
    
  per_ip:
    unauthenticated:
      requests_per_minute: 30
      burst_size: 50
    authenticated:
      requests_per_minute: 600
      burst_size: 1000
      
  per_organization:
    tier_free:
      requests_per_hour: 1000
      campaigns_per_day: 10
    tier_pro:
      requests_per_hour: 10000
      campaigns_per_day: 100
    tier_enterprise:
      requests_per_hour: 100000
      campaigns_per_day: unlimited
      
  per_endpoint:
    "/api/mailing/campaigns/{uid}/send":
      requests_per_minute: 10
      burst_size: 5
    "/api/mailing/subscribers/import":
      requests_per_hour: 10
      concurrent: 1
```

### 5.2 Input Validation

```go
// internal/validation/validator.go

type Validator struct {
    validate *validator.Validate
}

func (v *Validator) ValidateStruct(s interface{}) error {
    return v.validate.Struct(s)
}

// Example request validation
type CreateCampaignRequest struct {
    Name       string `json:"name" validate:"required,min=1,max=255"`
    Subject    string `json:"subject" validate:"required,min=1,max=500"`
    FromEmail  string `json:"from_email" validate:"required,email"`
    FromName   string `json:"from_name" validate:"required,min=1,max=255"`
    ListUID    string `json:"list_uid" validate:"required,uuid4"`
    TemplateUID string `json:"template_uid" validate:"omitempty,uuid4"`
    Content    *CampaignContent `json:"content" validate:"omitempty,dive"`
}

// SQL injection prevention
func (r *Repository) FindByEmail(ctx context.Context, email string) (*Subscriber, error) {
    // ALWAYS use parameterized queries
    query := `SELECT * FROM mailing_subscribers WHERE email = $1 AND organization_id = $2`
    return r.queryOne(ctx, query, email, GetOrganizationID(ctx))
}

// XSS prevention in template rendering
func (s *TemplateService) RenderHTML(template string, data map[string]interface{}) (string, error) {
    // Use html/template which escapes by default
    tmpl, err := htmltemplate.New("email").Parse(template)
    if err != nil {
        return "", err
    }
    
    var buf bytes.Buffer
    if err := tmpl.Execute(&buf, data); err != nil {
        return "", err
    }
    
    return buf.String(), nil
}
```

### 5.3 CORS Configuration

```go
// internal/api/cors.go

func CORSMiddleware(allowedOrigins []string) func(http.Handler) http.Handler {
    return cors.New(cors.Options{
        AllowedOrigins:   allowedOrigins,
        AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
        AllowedHeaders:   []string{"Authorization", "Content-Type", "X-API-Key", "X-Request-ID"},
        ExposedHeaders:   []string{"X-Request-ID", "X-RateLimit-Remaining"},
        AllowCredentials: true,
        MaxAge:           300,
    }).Handler
}

// Production config
var productionCORS = []string{
    "https://app.ignite.com",
    "https://www.ignite.com",
}
```

### 5.4 Request/Response Security Headers

```go
func SecurityHeadersMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        // Prevent clickjacking
        w.Header().Set("X-Frame-Options", "DENY")
        
        // Prevent MIME type sniffing
        w.Header().Set("X-Content-Type-Options", "nosniff")
        
        // Enable XSS protection
        w.Header().Set("X-XSS-Protection", "1; mode=block")
        
        // Content Security Policy
        w.Header().Set("Content-Security-Policy", 
            "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'")
        
        // Strict Transport Security
        w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
        
        // Referrer Policy
        w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
        
        next.ServeHTTP(w, r)
    })
}
```

---

## 6. Infrastructure Security

### 6.1 Network Security

```yaml
vpc_configuration:
  cidr: "10.0.0.0/16"
  
  subnets:
    public:
      - cidr: "10.0.1.0/24"
        az: "us-east-1a"
        purpose: "Load balancers, NAT gateways"
      - cidr: "10.0.2.0/24"
        az: "us-east-1b"
        purpose: "Load balancers, NAT gateways"
        
    private:
      - cidr: "10.0.10.0/24"
        az: "us-east-1a"
        purpose: "Application services"
      - cidr: "10.0.11.0/24"
        az: "us-east-1b"
        purpose: "Application services"
        
    database:
      - cidr: "10.0.20.0/24"
        az: "us-east-1a"
        purpose: "RDS, ElastiCache"
      - cidr: "10.0.21.0/24"
        az: "us-east-1b"
        purpose: "RDS, ElastiCache"

security_groups:
  alb:
    inbound:
      - port: 443
        source: "0.0.0.0/0"
        protocol: tcp
    outbound:
      - port: 8080
        destination: "app-sg"
        protocol: tcp
        
  app:
    inbound:
      - port: 8080
        source: "alb-sg"
        protocol: tcp
    outbound:
      - port: 5432
        destination: "db-sg"
        protocol: tcp
      - port: 6379
        destination: "cache-sg"
        protocol: tcp
      - port: 443
        destination: "0.0.0.0/0"
        protocol: tcp
        
  db:
    inbound:
      - port: 5432
        source: "app-sg"
        protocol: tcp
    outbound: []
```

### 6.2 Container Security

```dockerfile
# Secure Dockerfile template
FROM golang:1.22-alpine AS builder

# Don't run as root during build
RUN adduser -D -g '' builder
USER builder

WORKDIR /app
COPY --chown=builder:builder go.mod go.sum ./
RUN go mod download
COPY --chown=builder:builder . .
RUN CGO_ENABLED=0 go build -ldflags="-w -s" -o /app/service ./cmd/service

# Runtime image
FROM gcr.io/distroless/static-debian12:nonroot

# Copy binary with correct ownership
COPY --from=builder --chown=nonroot:nonroot /app/service /service

# Run as non-root
USER nonroot:nonroot

# No shell, no package manager, minimal attack surface
ENTRYPOINT ["/service"]
```

### 6.3 Audit Logging

```go
// internal/audit/logger.go

type AuditLogger struct {
    store AuditStore
}

type AuditEvent struct {
    ID             string                 `json:"id"`
    Timestamp      time.Time              `json:"timestamp"`
    OrganizationID int64                  `json:"organization_id"`
    UserID         int64                  `json:"user_id,omitempty"`
    APIKeyID       int64                  `json:"api_key_id,omitempty"`
    Action         string                 `json:"action"`
    ResourceType   string                 `json:"resource_type"`
    ResourceID     string                 `json:"resource_id"`
    OldValues      map[string]interface{} `json:"old_values,omitempty"`
    NewValues      map[string]interface{} `json:"new_values,omitempty"`
    IPAddress      string                 `json:"ip_address"`
    UserAgent      string                 `json:"user_agent"`
    RequestID      string                 `json:"request_id"`
    Success        bool                   `json:"success"`
    ErrorMessage   string                 `json:"error_message,omitempty"`
}

func (l *AuditLogger) Log(ctx context.Context, event *AuditEvent) error {
    event.ID = uuid.New().String()
    event.Timestamp = time.Now().UTC()
    event.RequestID = GetRequestID(ctx)
    
    // Redact sensitive fields
    event.OldValues = redactSensitive(event.OldValues)
    event.NewValues = redactSensitive(event.NewValues)
    
    return l.store.Save(ctx, event)
}

var sensitiveFields = []string{
    "password", "api_key", "secret", "token",
}

func redactSensitive(values map[string]interface{}) map[string]interface{} {
    if values == nil {
        return nil
    }
    
    result := make(map[string]interface{})
    for k, v := range values {
        for _, sensitive := range sensitiveFields {
            if strings.Contains(strings.ToLower(k), sensitive) {
                result[k] = "[REDACTED]"
                continue
            }
        }
        result[k] = v
    }
    return result
}
```

---

## 7. Compliance

### 7.1 GDPR Compliance

```yaml
gdpr:
  data_subject_rights:
    - right_of_access:
        endpoint: "GET /api/privacy/data-export"
        format: "JSON"
        response_time: "30 days"
        
    - right_to_erasure:
        endpoint: "DELETE /api/privacy/data"
        scope: "All personal data"
        response_time: "30 days"
        
    - right_to_rectification:
        endpoint: "PUT /api/subscribers/{uid}"
        scope: "Subscriber data"
        
    - right_to_data_portability:
        endpoint: "GET /api/privacy/data-export"
        format: "JSON, CSV"
        
  data_retention:
    subscriber_data: "Until deletion requested"
    tracking_events: "90 days"
    delivery_logs: "90 days"
    audit_logs: "7 years"
    
  consent_management:
    double_opt_in: "Required by default"
    unsubscribe: "One-click unsubscribe"
    consent_tracking: "Timestamp + IP recorded"
```

### 7.2 CAN-SPAM Compliance

```yaml
can_spam:
  requirements:
    - physical_address:
        required: true
        source: "List company information"
        
    - unsubscribe_mechanism:
        required: true
        method: "One-click unsubscribe link"
        processing_time: "10 business days max"
        
    - accurate_header:
        from_address: "Must be valid"
        subject_line: "No deception"
        
    - identification:
        message_type: "Clear commercial intent"
        
  implementation:
    - All templates include unsubscribe link
    - Physical address auto-inserted
    - From address validated against sending domains
    - Subject line reviewed for spam triggers
```

### 7.3 Data Retention Policy

```yaml
data_retention:
  user_data:
    active_users: "Indefinite"
    deleted_users: "30 days then hard delete"
    
  subscriber_data:
    active: "Indefinite"
    unsubscribed: "90 days then archive"
    bounced: "90 days then archive"
    
  campaign_data:
    draft: "Until deleted by user"
    sent: "Indefinite"
    
  tracking_events:
    opens: "90 days"
    clicks: "90 days"
    
  delivery_logs:
    success: "30 days"
    failure: "90 days"
    
  audit_logs:
    all: "7 years"
    
  implementation:
    automated_cleanup: true
    cleanup_schedule: "Daily at 02:00 UTC"
    archive_storage: "S3 Glacier"
```

---

## 8. Incident Response

### 8.1 Security Incident Procedure

```yaml
incident_response:
  severity_levels:
    critical:
      definition: "Data breach, system compromise, service outage"
      response_time: "15 minutes"
      escalation: "Immediate to all stakeholders"
      
    high:
      definition: "Potential data exposure, vulnerability exploited"
      response_time: "1 hour"
      escalation: "Security team + management"
      
    medium:
      definition: "Failed attack attempts, suspicious activity"
      response_time: "4 hours"
      escalation: "Security team"
      
    low:
      definition: "Minor security issues, policy violations"
      response_time: "24 hours"
      escalation: "Log and review"
      
  response_steps:
    1_identify:
      - Confirm incident is real
      - Determine scope and impact
      - Assign severity level
      
    2_contain:
      - Isolate affected systems
      - Block malicious traffic
      - Preserve evidence
      
    3_eradicate:
      - Remove threat
      - Patch vulnerabilities
      - Strengthen defenses
      
    4_recover:
      - Restore systems
      - Verify functionality
      - Monitor for recurrence
      
    5_lessons_learned:
      - Document incident
      - Update procedures
      - Implement improvements
```

### 8.2 Security Monitoring

```yaml
monitoring:
  alerts:
    - name: "Failed login attempts"
      condition: "5+ failures in 5 minutes"
      action: "Block IP, notify security"
      
    - name: "API key abuse"
      condition: "Rate limit exceeded by 10x"
      action: "Revoke key, notify owner"
      
    - name: "SQL injection attempt"
      condition: "WAF blocks malicious query"
      action: "Log, analyze, block IP if repeated"
      
    - name: "Unusual data export"
      condition: "Export > 10x normal"
      action: "Notify admin, flag for review"
      
    - name: "Service account login"
      condition: "Login outside business hours"
      action: "MFA challenge, notify"
      
  dashboards:
    - Security Overview
    - Authentication Activity
    - API Usage Patterns
    - WAF Blocks
    - Audit Log Analysis
```

---

**Document End**

*This security architecture ensures enterprise-grade protection for the Ignite Mailing Platform.*
