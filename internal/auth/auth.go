package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/config"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// GoogleUserInfo represents the user info returned by Google
type GoogleUserInfo struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	VerifiedEmail bool   `json:"verified_email"`
	Name          string `json:"name"`
	GivenName     string `json:"given_name"`
	FamilyName    string `json:"family_name"`
	Picture       string `json:"picture"`
	HD            string `json:"hd"` // Hosted domain (GSuite domain)
}

// Session represents an authenticated user session
type Session struct {
	UserID    string    `json:"user_id"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	Picture   string    `json:"picture"`
	Domain    string    `json:"domain"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// AuthManager handles Google OAuth authentication
type AuthManager struct {
	config       *config.AuthConfig
	oauth2Config *oauth2.Config
	sessions     map[string]*Session
	sessionMu    sync.RWMutex
	baseURL      string
}

// NewAuthManager creates a new authentication manager
func NewAuthManager(cfg *config.AuthConfig, baseURL string) *AuthManager {
	oauth2Config := &oauth2.Config{
		ClientID:     cfg.GoogleClientID,
		ClientSecret: cfg.GoogleClientSecret,
		RedirectURL:  baseURL + "/auth/callback",
		Scopes: []string{
			"https://www.googleapis.com/auth/userinfo.email",
			"https://www.googleapis.com/auth/userinfo.profile",
		},
		Endpoint: google.Endpoint,
	}

	return &AuthManager{
		config:       cfg,
		oauth2Config: oauth2Config,
		sessions:     make(map[string]*Session),
		baseURL:      baseURL,
	}
}

// generateState creates a random state string for OAuth
func generateState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(b), nil
}

// generateSessionID creates a random session ID
func generateSessionID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(b), nil
}

// HandleLogin initiates the Google OAuth flow
func (am *AuthManager) HandleLogin(w http.ResponseWriter, r *http.Request) {
	state, err := generateState()
	if err != nil {
		http.Error(w, "Failed to generate state", http.StatusInternalServerError)
		return
	}

	// Store state in a cookie for verification
	http.SetCookie(w, &http.Cookie{
		Name:     "oauth_state",
		Value:    state,
		Path:     "/",
		MaxAge:   300, // 5 minutes
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	// Add hd parameter to restrict to the allowed domain
	url := am.oauth2Config.AuthCodeURL(state, oauth2.AccessTypeOnline) + "&hd=" + am.config.AllowedDomain
	http.Redirect(w, r, url, http.StatusTemporaryRedirect)
}

// HandleCallback processes the OAuth callback from Google
func (am *AuthManager) HandleCallback(w http.ResponseWriter, r *http.Request) {
	// Verify state
	stateCookie, err := r.Cookie("oauth_state")
	if err != nil {
		log.Printf("Auth: No state cookie found: %v", err)
		http.Redirect(w, r, "/?error=invalid_state", http.StatusTemporaryRedirect)
		return
	}

	if r.URL.Query().Get("state") != stateCookie.Value {
		log.Printf("Auth: State mismatch")
		http.Redirect(w, r, "/?error=invalid_state", http.StatusTemporaryRedirect)
		return
	}

	// Clear state cookie
	http.SetCookie(w, &http.Cookie{
		Name:   "oauth_state",
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})

	// Check for errors from Google
	if errMsg := r.URL.Query().Get("error"); errMsg != "" {
		log.Printf("Auth: Google returned error: %s", errMsg)
		http.Redirect(w, r, "/?error="+errMsg, http.StatusTemporaryRedirect)
		return
	}

	// Exchange code for token
	code := r.URL.Query().Get("code")
	token, err := am.oauth2Config.Exchange(context.Background(), code)
	if err != nil {
		log.Printf("Auth: Failed to exchange code: %v", err)
		http.Redirect(w, r, "/?error=exchange_failed", http.StatusTemporaryRedirect)
		return
	}

	// Get user info from Google
	userInfo, err := am.getUserInfo(token.AccessToken)
	if err != nil {
		log.Printf("Auth: Failed to get user info: %v", err)
		http.Redirect(w, r, "/?error=userinfo_failed", http.StatusTemporaryRedirect)
		return
	}

	// Verify domain
	emailDomain := strings.Split(userInfo.Email, "@")
	if len(emailDomain) != 2 || emailDomain[1] != am.config.AllowedDomain {
		log.Printf("Auth: Domain not allowed: %s (expected %s)", userInfo.Email, am.config.AllowedDomain)
		http.Redirect(w, r, "/?error=domain_not_allowed", http.StatusTemporaryRedirect)
		return
	}

	// Create session
	sessionID, err := generateSessionID()
	if err != nil {
		log.Printf("Auth: Failed to generate session ID: %v", err)
		http.Redirect(w, r, "/?error=session_failed", http.StatusTemporaryRedirect)
		return
	}

	session := &Session{
		UserID:    userInfo.ID,
		Email:     userInfo.Email,
		Name:      userInfo.Name,
		Picture:   userInfo.Picture,
		Domain:    userInfo.HD,
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(time.Duration(am.config.CookieMaxAge) * time.Second),
	}

	am.sessionMu.Lock()
	am.sessions[sessionID] = session
	am.sessionMu.Unlock()

	log.Printf("Auth: User logged in: %s (%s)", userInfo.Email, userInfo.Name)

	// Set session cookie
	http.SetCookie(w, &http.Cookie{
		Name:     am.config.CookieName,
		Value:    sessionID,
		Path:     "/",
		MaxAge:   am.config.CookieMaxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
}

// HandleLogout logs out the user
func (am *AuthManager) HandleLogout(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(am.config.CookieName)
	if err == nil {
		am.sessionMu.Lock()
		delete(am.sessions, cookie.Value)
		am.sessionMu.Unlock()
	}

	http.SetCookie(w, &http.Cookie{
		Name:   am.config.CookieName,
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})

	http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
}

// HandleUserInfo returns the current user's info as JSON
func (am *AuthManager) HandleUserInfo(w http.ResponseWriter, r *http.Request) {
	session := am.GetSession(r)
	if session == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"authenticated": false,
		})
		return
	}

	// Use default organization context - actual org lookup happens in API middleware
	// The org ID is passed via X-Organization-ID header or org_id query param
	orgName := session.Domain + " Organization"
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"authenticated": true,
		"user": map[string]string{
			"id":      session.UserID,
			"email":   session.Email,
			"name":    session.Name,
			"picture": session.Picture,
			"domain":  session.Domain,
		},
		"organization": map[string]string{
			"name":   orgName,
			"domain": session.Domain,
		},
	})
}

// GetSession returns the session for the current request, or nil if not authenticated
func (am *AuthManager) GetSession(r *http.Request) *Session {
	cookie, err := r.Cookie(am.config.CookieName)
	if err != nil {
		return nil
	}

	am.sessionMu.RLock()
	session, exists := am.sessions[cookie.Value]
	am.sessionMu.RUnlock()

	if !exists {
		return nil
	}

	// Check if session has expired
	if time.Now().After(session.ExpiresAt) {
		am.sessionMu.Lock()
		delete(am.sessions, cookie.Value)
		am.sessionMu.Unlock()
		return nil
	}

	return session
}

// IsAuthenticated checks if the request is from an authenticated user
func (am *AuthManager) IsAuthenticated(r *http.Request) bool {
	return am.GetSession(r) != nil
}

// RequireAuth is middleware that requires authentication
func (am *AuthManager) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Allow auth endpoints without authentication
		if strings.HasPrefix(r.URL.Path, "/auth/") {
			next.ServeHTTP(w, r)
			return
		}

		// Allow health endpoint without authentication
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}

		// Check if authenticated
		if !am.IsAuthenticated(r) {
			// For API requests, return 401
			if strings.HasPrefix(r.URL.Path, "/api/") {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				json.NewEncoder(w).Encode(map[string]string{
					"error": "unauthorized",
				})
				return
			}

			// For other requests, serve the login page (let frontend handle it)
			next.ServeHTTP(w, r)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// getUserInfo fetches the user's profile from Google
func (am *AuthManager) getUserInfo(accessToken string) (*GoogleUserInfo, error) {
	resp, err := http.Get("https://www.googleapis.com/oauth2/v2/userinfo?access_token=" + accessToken)
	if err != nil {
		return nil, fmt.Errorf("failed to get user info: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("google API error: %s", string(body))
	}

	var userInfo GoogleUserInfo
	if err := json.Unmarshal(body, &userInfo); err != nil {
		return nil, fmt.Errorf("failed to parse user info: %w", err)
	}

	return &userInfo, nil
}

// ValidateCredentials performs a lightweight check against Google's token
// endpoint to verify the OAuth client ID and secret are valid. This catches
// stale/rotated credentials at boot instead of at first user login.
func (am *AuthManager) ValidateCredentials(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// Use the token endpoint with an invalid grant to provoke a distinguishable error.
	// Google returns "invalid_client" for bad credentials vs "invalid_grant" for
	// bad code — so we can tell the difference.
	vals := fmt.Sprintf("grant_type=authorization_code&code=validation_probe&client_id=%s&client_secret=%s&redirect_uri=%s",
		am.oauth2Config.ClientID, am.oauth2Config.ClientSecret, am.oauth2Config.RedirectURL)

	req, err := http.NewRequestWithContext(ctx, "POST", google.Endpoint.TokenURL, strings.NewReader(vals))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("token endpoint unreachable: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	// "invalid_grant" or "invalid_request" means the client itself is fine,
	// but the dummy code is (obviously) wrong — that's the success case.
	if strings.Contains(bodyStr, "invalid_grant") || strings.Contains(bodyStr, "invalid_request") || strings.Contains(bodyStr, "redirect_uri_mismatch") {
		return nil
	}

	// "invalid_client" means the client ID or secret is wrong.
	if strings.Contains(bodyStr, "invalid_client") {
		return fmt.Errorf("Google OAuth credentials are INVALID (client_id or client_secret rejected by Google). "+
			"Verify in GCP Console → APIs & Services → Credentials for project %s", am.oauth2Config.ClientID[:12]+"...")
	}

	return fmt.Errorf("unexpected response from Google token endpoint (HTTP %d): %s", resp.StatusCode, bodyStr)
}

// CleanupExpiredSessions removes expired sessions periodically
func (am *AuthManager) CleanupExpiredSessions() {
	ticker := time.NewTicker(5 * time.Minute)
	go func() {
		for range ticker.C {
			am.sessionMu.Lock()
			now := time.Now()
			for id, session := range am.sessions {
				if now.After(session.ExpiresAt) {
					delete(am.sessions, id)
				}
			}
			am.sessionMu.Unlock()
		}
	}()
}
