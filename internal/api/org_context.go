package api

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"sync"

	"github.com/google/uuid"
)

// OrgContextKey is the key for storing organization context
type OrgContextKey struct{}

// UserContextKey is the key for storing user context
type UserContextKey struct{}

// OrganizationContext holds organization information extracted from auth
type OrganizationContext struct {
	ID       uuid.UUID
	Name     string
	Domain   string
	Settings map[string]interface{}
}

// UserContext holds user information extracted from auth
type UserContext struct {
	ID    uuid.UUID
	Email string
	Name  string
	Role  string
}

// OrgContextProvider provides organization context extraction
type OrgContextProvider struct {
	db              *sql.DB
	defaultOrgID    uuid.UUID
	defaultOrgName  string
	orgCache        map[string]*OrganizationContext
	cacheMutex      sync.RWMutex
	devModeEnabled  bool
}

// NewOrgContextProvider creates a new provider
func NewOrgContextProvider(db *sql.DB) *OrgContextProvider {
	// Check for dev mode via environment
	devMode := os.Getenv("DEV_MODE") == "true" || os.Getenv("ENVIRONMENT") == "development"
	
	// Get default org from environment or use a proper UUID
	defaultOrgIDStr := os.Getenv("DEFAULT_ORG_ID")
	var defaultOrgID uuid.UUID
	if defaultOrgIDStr != "" {
		parsed, err := uuid.Parse(defaultOrgIDStr)
		if err == nil {
			defaultOrgID = parsed
		}
	}
	
	defaultOrgName := os.Getenv("DEFAULT_ORG_NAME")
	if defaultOrgName == "" {
		defaultOrgName = "Default Organization"
	}
	
	return &OrgContextProvider{
		db:             db,
		defaultOrgID:   defaultOrgID,
		defaultOrgName: defaultOrgName,
		orgCache:       make(map[string]*OrganizationContext),
		devModeEnabled: devMode,
	}
}

// ExtractOrgID extracts organization ID from request with proper fallback chain
// Priority: 1. Auth header/token, 2. X-Organization-ID header, 3. Query param, 4. Session, 5. Dev mode default
func (p *OrgContextProvider) ExtractOrgID(r *http.Request) (uuid.UUID, error) {
	ctx := r.Context()
	
	// 1. Check if already in context (from auth middleware)
	if orgCtx, ok := ctx.Value(OrgContextKey{}).(*OrganizationContext); ok && orgCtx != nil {
		return orgCtx.ID, nil
	}
	
	// 2. Check X-Organization-ID header
	if orgIDStr := r.Header.Get("X-Organization-ID"); orgIDStr != "" {
		orgID, err := uuid.Parse(orgIDStr)
		if err == nil {
			return orgID, nil
		}
	}
	
	// 3. Check query parameter
	if orgIDStr := r.URL.Query().Get("org_id"); orgIDStr != "" {
		orgID, err := uuid.Parse(orgIDStr)
		if err == nil {
			return orgID, nil
		}
	}
	
	// 4. Check authorization header and extract org from token
	if authHeader := r.Header.Get("Authorization"); authHeader != "" {
		orgID, err := p.extractOrgFromAuth(r.Context(), authHeader)
		if err == nil {
			return orgID, nil
		}
	}
	
	// 5. Dev mode fallback
	if p.devModeEnabled && p.defaultOrgID != uuid.Nil {
		return p.defaultOrgID, nil
	}
	
	return uuid.Nil, fmt.Errorf("organization ID not found in request")
}

// ExtractOrgIDString returns org ID as string
func (p *OrgContextProvider) ExtractOrgIDString(r *http.Request) (string, error) {
	orgID, err := p.ExtractOrgID(r)
	if err != nil {
		return "", err
	}
	return orgID.String(), nil
}

// MustExtractOrgID extracts org ID, returns zero UUID on error (for backward compatibility)
func (p *OrgContextProvider) MustExtractOrgID(r *http.Request) uuid.UUID {
	orgID, err := p.ExtractOrgID(r)
	if err != nil {
		return uuid.Nil
	}
	return orgID
}

// MustExtractOrgIDString extracts org ID as string, returns empty on error
func (p *OrgContextProvider) MustExtractOrgIDString(r *http.Request) string {
	orgID, err := p.ExtractOrgID(r)
	if err != nil {
		return ""
	}
	return orgID.String()
}

// extractOrgFromAuth extracts organization from authorization token
func (p *OrgContextProvider) extractOrgFromAuth(ctx context.Context, authHeader string) (uuid.UUID, error) {
	// This would integrate with your auth system
	// For now, return error to fall through to other methods
	return uuid.Nil, fmt.Errorf("auth extraction not implemented")
}

// GetOrganization retrieves full organization context
func (p *OrgContextProvider) GetOrganization(ctx context.Context, orgID uuid.UUID) (*OrganizationContext, error) {
	// Check cache first
	p.cacheMutex.RLock()
	if cached, ok := p.orgCache[orgID.String()]; ok {
		p.cacheMutex.RUnlock()
		return cached, nil
	}
	p.cacheMutex.RUnlock()
	
	// Query database
	var org OrganizationContext
	var settingsJSON sql.NullString
	err := p.db.QueryRowContext(ctx, `
		SELECT id, name, COALESCE(domain, '') as domain, COALESCE(settings::text, '{}') as settings
		FROM organizations 
		WHERE id = $1
	`, orgID).Scan(&org.ID, &org.Name, &org.Domain, &settingsJSON)
	
	if err != nil {
		return nil, err
	}
	
	// Cache the result
	p.cacheMutex.Lock()
	p.orgCache[orgID.String()] = &org
	p.cacheMutex.Unlock()
	
	return &org, nil
}

// OrgContextMiddleware injects organization context into requests
func (p *OrgContextProvider) OrgContextMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		orgID, err := p.ExtractOrgID(r)
		if err == nil && orgID != uuid.Nil {
			org, err := p.GetOrganization(r.Context(), orgID)
			if err == nil {
				ctx := context.WithValue(r.Context(), OrgContextKey{}, org)
				r = r.WithContext(ctx)
			}
		}
		next.ServeHTTP(w, r)
	})
}

// RequireOrgMiddleware requires organization context, returns 401 if not present
func (p *OrgContextProvider) RequireOrgMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		orgID, err := p.ExtractOrgID(r)
		if err != nil || orgID == uuid.Nil {
			http.Error(w, `{"error":"organization context required"}`, http.StatusUnauthorized)
			return
		}
		
		org, err := p.GetOrganization(r.Context(), orgID)
		if err != nil {
			http.Error(w, `{"error":"organization not found"}`, http.StatusUnauthorized)
			return
		}
		
		ctx := context.WithValue(r.Context(), OrgContextKey{}, org)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// GetOrgFromContext retrieves organization from context
func GetOrgFromContext(ctx context.Context) *OrganizationContext {
	if org, ok := ctx.Value(OrgContextKey{}).(*OrganizationContext); ok {
		return org
	}
	return nil
}

// GetOrgIDFromContext retrieves organization ID from context
func GetOrgIDFromContext(ctx context.Context) uuid.UUID {
	if org := GetOrgFromContext(ctx); org != nil {
		return org.ID
	}
	return uuid.Nil
}

// GetUserFromContext retrieves user from context
func GetUserFromContext(ctx context.Context) *UserContext {
	if user, ok := ctx.Value(UserContextKey{}).(*UserContext); ok {
		return user
	}
	return nil
}

// GetUserIDFromContext retrieves user ID from context
func GetUserIDFromContext(ctx context.Context) uuid.UUID {
	if user := GetUserFromContext(ctx); user != nil {
		return user.ID
	}
	return uuid.Nil
}

// GetOrgIDFromRequest extracts org ID from request with proper fallback chain
// This is a standalone function that can be used without OrgContextProvider instance
// Priority: 1. Context (from middleware), 2. X-Organization-ID header, 3. Query param, 4. Dev mode env var
func GetOrgIDFromRequest(r *http.Request) (uuid.UUID, error) {
	ctx := r.Context()

	// 1. Check context (from middleware)
	if org := GetOrgFromContext(ctx); org != nil {
		return org.ID, nil
	}

	// 2. Check X-Organization-ID header
	if orgIDStr := r.Header.Get("X-Organization-ID"); orgIDStr != "" {
		orgID, err := uuid.Parse(orgIDStr)
		if err == nil {
			return orgID, nil
		}
	}

	// 3. Check query parameter
	if orgIDStr := r.URL.Query().Get("org_id"); orgIDStr != "" {
		orgID, err := uuid.Parse(orgIDStr)
		if err == nil {
			return orgID, nil
		}
	}

	// 4. Dev mode fallback from environment
	devMode := os.Getenv("DEV_MODE") == "true" || os.Getenv("ENVIRONMENT") == "development"
	if devMode {
		if defaultOrgIDStr := os.Getenv("DEFAULT_ORG_ID"); defaultOrgIDStr != "" {
			orgID, err := uuid.Parse(defaultOrgIDStr)
			if err == nil {
				return orgID, nil
			}
		}
	}

	return uuid.Nil, fmt.Errorf("organization ID not found in request")
}

// GetOrgIDStringFromRequest extracts org ID as string from request
func GetOrgIDStringFromRequest(r *http.Request) (string, error) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		return "", err
	}
	return orgID.String(), nil
}

// MustGetOrgIDFromRequest extracts org ID, returns Nil UUID on error (for backward compatibility during migration)
func MustGetOrgIDFromRequest(r *http.Request) uuid.UUID {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		return uuid.Nil
	}
	return orgID
}

// MustGetOrgIDStringFromRequest extracts org ID as string, returns empty string on error
func MustGetOrgIDStringFromRequest(r *http.Request) string {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		return ""
	}
	return orgID.String()
}
