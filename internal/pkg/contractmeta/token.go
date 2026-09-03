package contractmeta

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"time"
)

// KeyEnvVar is the environment variable carrying the HMAC key (§1.5, manifest
// entry CONTRACT_TOKEN_KEY).
const KeyEnvVar = "CONTRACT_TOKEN_KEY"

// MinKeyLen is the shortest key this package will use. HMAC-SHA256 accepts any
// length, but this key gates whether a contract may govern live sending, so a
// short key is treated as a misconfiguration rather than silently accepted.
const MinKeyLen = 32

var (
	// ErrNoKey means CONTRACT_TOKEN_KEY is unset, empty, or too short. Every
	// caller treats it as fail-closed: no token issued, no contract honoured.
	ErrNoKey = errors.New("contractmeta: " + KeyEnvVar + " is not set")
	// ErrNoToken means the row carries no token at all — it never went through
	// the approved → scheduled issue path, so it cannot be honoured.
	ErrNoToken = errors.New("contractmeta: contract carries no token")
	// ErrUnsupportedAlg means the token names an algorithm this build cannot verify.
	ErrUnsupportedAlg = errors.New("contractmeta: unsupported token algorithm")
	// ErrTokenMismatch means the token does not match the body it is stamped on
	// — the row was edited outside the sanctioned path.
	ErrTokenMismatch = errors.New("contractmeta: token does not match contract body")
)

// KeyFromEnv reads the HMAC key from CONTRACT_TOKEN_KEY. It returns ErrNoKey
// when the variable is unset, empty, or shorter than MinKeyLen — there is no
// built-in fallback key, by design: a default would make every token forgeable
// by anyone reading this source.
func KeyFromEnv() ([]byte, error) {
	v := os.Getenv(KeyEnvVar)
	if v == "" {
		return nil, ErrNoKey
	}
	if len(v) < MinKeyLen {
		return nil, fmt.Errorf("%w: key is %d bytes, need at least %d", ErrNoKey, len(v), MinKeyLen)
	}
	return []byte(v), nil
}

// Issue stamps a token over the canonical bytes. With no key it returns the
// ZERO Token — never a token computed under an empty key — so a caller that
// forgot to check KeyFromEnv writes an unusable stamp instead of a forgeable one.
func Issue(key []byte, canonical []byte, now time.Time) Token {
	if len(key) == 0 {
		return Token{}
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(canonical)
	return Token{
		Alg:      AlgHMACSHA256,
		IssuedAt: now.UTC(),
		IssuedBy: IssuerSystem,
		Value:    hex.EncodeToString(mac.Sum(nil)),
	}
}

// Verify checks a token against the canonical bytes of the body it claims to
// stamp. It returns nil only when the key, the algorithm and the MAC all agree.
func Verify(key []byte, canonical []byte, tok Token) error {
	if len(key) == 0 {
		return ErrNoKey
	}
	if tok.Value == "" {
		return ErrNoToken
	}
	if tok.Alg != AlgHMACSHA256 {
		return fmt.Errorf("%w: %q", ErrUnsupportedAlg, tok.Alg)
	}
	want, err := hex.DecodeString(tok.Value)
	if err != nil {
		return fmt.Errorf("%w: token value is not hex: %v", ErrTokenMismatch, err)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(canonical)
	if !hmac.Equal(mac.Sum(nil), want) {
		return ErrTokenMismatch
	}
	return nil
}
