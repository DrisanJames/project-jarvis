package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Onboarding tokens let a verified Google account that is NOT allowed into the
// portal fill in the new-customer form. They are not sessions: nothing in
// GetSession, RequireAuth or apiAuthMiddleware reads them, and the only
// consumer is POST /api/public/onboarding (internal/api/marketing_leads.go).

const (
	onboardingTokenPurpose = "onboarding"
	onboardingTokenTTL     = 30 * time.Minute
)

// ErrOnboardingToken covers every rejected token; callers answer 401.
var ErrOnboardingToken = errors.New("invalid onboarding token")

// OnboardingClaims are the Google-verified facts carried by the token. The
// browser reads Name and Email for prefill; the server trusts them only after
// VerifyOnboardingToken.
type OnboardingClaims struct {
	Purpose string `json:"p"`
	Email   string `json:"e"`
	Name    string `json:"n"`
	Subject string `json:"s"`
	Expires int64  `json:"x"`
}

// DerivedKey returns a purpose-bound HMAC key derived from the OAuth client
// secret, or nil when none is configured (callers fail closed). The separate
// purpose strings mean a token minted for one use never verifies for another.
func (am *AuthManager) DerivedKey(purpose string) []byte {
	if am == nil || am.config == nil || am.config.GoogleClientSecret == "" {
		return nil
	}
	h := hmac.New(sha256.New, []byte(am.config.GoogleClientSecret))
	h.Write([]byte("jarvis-derived-v1:" + purpose))
	return h.Sum(nil)
}

func (am *AuthManager) onboardingSig(payload string) []byte {
	h := hmac.New(sha256.New, am.DerivedKey("onboarding-token"))
	h.Write([]byte(payload))
	return h.Sum(nil)
}

// IssueOnboardingToken signs a 30-minute token for a Google-verified identity.
func (am *AuthManager) IssueOnboardingToken(u *GoogleUserInfo, now time.Time) (string, error) {
	if u == nil || !u.VerifiedEmail || u.Email == "" {
		return "", ErrOnboardingToken
	}
	if am.DerivedKey("onboarding-token") == nil {
		return "", errors.New("auth secret not configured")
	}
	raw, err := json.Marshal(OnboardingClaims{
		Purpose: onboardingTokenPurpose,
		Email:   strings.ToLower(strings.TrimSpace(u.Email)),
		Name:    u.Name,
		Subject: u.ID,
		Expires: now.Add(onboardingTokenTTL).Unix(),
	})
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	return payload + "." + base64.RawURLEncoding.EncodeToString(am.onboardingSig(payload)), nil
}

// VerifyOnboardingToken checks signature, purpose and expiry.
func (am *AuthManager) VerifyOnboardingToken(token string, now time.Time) (*OnboardingClaims, error) {
	if am.DerivedKey("onboarding-token") == nil {
		return nil, ErrOnboardingToken
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return nil, ErrOnboardingToken
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || subtle.ConstantTimeCompare(sig, am.onboardingSig(parts[0])) != 1 {
		return nil, ErrOnboardingToken
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, ErrOnboardingToken
	}
	var c OnboardingClaims
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, ErrOnboardingToken
	}
	if c.Purpose != onboardingTokenPurpose || c.Email == "" || now.Unix() >= c.Expires {
		return nil, ErrOnboardingToken
	}
	return &c, nil
}
