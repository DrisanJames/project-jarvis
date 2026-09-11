// Package preferences owns the recipient-facing email-preference model: the
// signed v1 link token, the mailing_subscriber_preferences store, and the
// in-memory ShouldSend hub the send paths consult.
//
// It is a leaf package (stdlib + uuid only) so the send worker, the API
// handlers and the mailing context builder can all mint and verify the SAME
// token without an import cycle.
package preferences

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Scope is what an unsubscribe carried by a token applies to.
type Scope string

const (
	// ScopeBrand = the brand root named in the token only (brands are
	// separate senders — operator doctrine 2026-07-13).
	ScopeBrand Scope = "brand"
	// ScopeAll = every brand of the organization (global suppression).
	ScopeAll Scope = "all"
)

const (
	tokenVersion = "v1"
	// TokenTTL is how long a minted link is valid. CAN-SPAM needs >= 30 days.
	TokenTTL = 180 * 24 * time.Hour
	// ExpiryGrace keeps a link working for a while past its exp, so mail
	// read late (or a clock-skewed issuer) still reaches the page instead of
	// the neutral "expired" state.
	ExpiryGrace = 30 * 24 * time.Hour

	macDomainToken = "prefs.v1.token|"
	macDomainNonce = "prefs.v1.nonce|"
)

var (
	ErrMalformed = errors.New("preferences token malformed")
	ErrSignature = errors.New("preferences token signature invalid")
	ErrExpired   = errors.New("preferences token expired")
	ErrNoSecret  = errors.New("preferences token secret not configured")

	brandRootRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)
)

// Claims is the decoded, verified content of a token.
type Claims struct {
	OrgID        string
	SubscriberID string
	BrandRoot    string // lowercase apex; "" only with ScopeAll
	Scope        Scope
	Exp          time.Time
}

// Mint builds a token for subscriberID valid for TokenTTL from now. Returns ""
// when secret is empty or the inputs are unusable — callers then emit a
// token-less link, which lands on the neutral page (never an unsigned grant).
func Mint(orgID, subscriberID, brandRoot string, scope Scope, now time.Time, secret string) string {
	tok, err := Sign(Claims{
		OrgID:        orgID,
		SubscriberID: subscriberID,
		BrandRoot:    brandRoot,
		Scope:        scope,
		Exp:          now.Add(TokenTTL),
	}, secret)
	if err != nil {
		return ""
	}
	return tok
}

// Sign encodes c as v1|org|subscriber|brand_root|scope|exp_unix (base64url,
// unpadded) followed by "." and the full-length hex HMAC-SHA256.
func Sign(c Claims, secret string) (string, error) {
	if secret == "" {
		return "", ErrNoSecret
	}
	c.BrandRoot = strings.ToLower(strings.TrimSpace(c.BrandRoot))
	if err := validate(c); err != nil {
		return "", err
	}
	payload := strings.Join([]string{
		tokenVersion, c.OrgID, c.SubscriberID, c.BrandRoot, string(c.Scope),
		strconv.FormatInt(c.Exp.Unix(), 10),
	}, "|")
	enc := base64.RawURLEncoding.EncodeToString([]byte(payload))
	return enc + "." + mac(secret, macDomainToken+enc), nil
}

// Verify checks signature, shape and expiry. On ErrExpired the returned
// Claims are populated (the signature WAS valid) so the caller can offer a
// fresh link for the right brand; on every other error Claims are zero.
func Verify(tok, secret string, now time.Time) (Claims, error) {
	if secret == "" {
		return Claims{}, ErrNoSecret
	}
	tok = strings.TrimSpace(tok)
	dot := strings.LastIndexByte(tok, '.')
	if dot <= 0 || dot == len(tok)-1 || len(tok) > 1024 {
		return Claims{}, ErrMalformed
	}
	enc, sig := tok[:dot], tok[dot+1:]
	if !hmac.Equal([]byte(sig), []byte(mac(secret, macDomainToken+enc))) {
		return Claims{}, ErrSignature
	}
	raw, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return Claims{}, ErrMalformed
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 6 || parts[0] != tokenVersion {
		return Claims{}, ErrMalformed
	}
	expUnix, err := strconv.ParseInt(parts[5], 10, 64)
	if err != nil {
		return Claims{}, ErrMalformed
	}
	c := Claims{
		OrgID:        parts[1],
		SubscriberID: parts[2],
		BrandRoot:    parts[3],
		Scope:        Scope(parts[4]),
		Exp:          time.Unix(expUnix, 0).UTC(),
	}
	if err := validate(c); err != nil {
		return Claims{}, err
	}
	if now.After(c.Exp.Add(ExpiryGrace)) {
		return c, ErrExpired
	}
	return c, nil
}

func validate(c Claims) error {
	if _, err := uuid.Parse(c.OrgID); err != nil {
		return ErrMalformed
	}
	if id, err := uuid.Parse(c.SubscriberID); err != nil || id == uuid.Nil {
		return ErrMalformed
	}
	switch c.Scope {
	case ScopeBrand:
		if !brandRootRe.MatchString(c.BrandRoot) {
			return ErrMalformed
		}
	case ScopeAll:
		if c.BrandRoot != "" && !brandRootRe.MatchString(c.BrandRoot) {
			return ErrMalformed
		}
	default:
		return ErrMalformed
	}
	return nil
}

// Nonce is the CSRF value embedded in the preference form. It is bound to the
// token and to an hour bucket; VerifyNonce accepts the current and previous
// bucket (a 1–2h window). No cookie is involved — a forger needs the token,
// which only the recipient's mailbox holds.
func Nonce(tok, secret string, now time.Time) string {
	return nonceFor(tok, secret, now.Unix()/3600)
}

// VerifyNonce reports whether n was issued for tok within the window.
func VerifyNonce(tok, n, secret string, now time.Time) bool {
	if secret == "" || n == "" || tok == "" {
		return false
	}
	b := now.Unix() / 3600
	for _, bucket := range []int64{b, b - 1} {
		if hmac.Equal([]byte(n), []byte(nonceFor(tok, secret, bucket))) {
			return true
		}
	}
	return false
}

func nonceFor(tok, secret string, bucket int64) string {
	if secret == "" || tok == "" {
		return ""
	}
	return mac(secret, macDomainNonce+tok+"|"+strconv.FormatInt(bucket, 10))[:32]
}

func mac(secret, data string) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(data))
	return hex.EncodeToString(h.Sum(nil))
}

// EmailHash is the store key: hex sha256 of lower(trim(email)).
func EmailHash(email string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
	return hex.EncodeToString(sum[:])
}
