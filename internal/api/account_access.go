package api

// The access gate: the optional per-user check on auth.AuthManager that makes a
// disabled user actually lose access. It sits on every session read, so answers
// are cached 60s. A DB failure lets the user through (the domain rule still
// holds) and is remembered briefly, so a slow DB costs one wait per user rather
// than one per request. A change on another ECS task takes effect within 60s.

import (
	"context"
	"log"
	"sync"
	"time"
)

const (
	accessCacheTTL    = 60 * time.Second
	accessFailOpenTTL = 10 * time.Second
	accessLookupLimit = 2 * time.Second
)

type accessEntry struct {
	allowed bool
	expires time.Time
}

type accessCache struct {
	mu      sync.Mutex
	entries map[string]accessEntry
}

func newAccessCache() *accessCache {
	return &accessCache{entries: map[string]accessEntry{}}
}

func (c *accessCache) get(email string, now time.Time) (allowed, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, found := c.entries[email]
	if !found || now.After(e.expires) {
		return false, false
	}
	return e.allowed, true
}

func (c *accessCache) put(email string, allowed bool, expires time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[email] = accessEntry{allowed: allowed, expires: expires}
}

func (c *accessCache) forget(email string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, normalizeAccountEmail(email))
}

// AccessAllowed is the gate installed with AuthManager.SetAccessGate. It denies
// an email disabled in any organization: sign-in is one domain, and the session
// carries no organization to narrow it by.
func (s *AccountService) AccessAllowed(email string) bool {
	email = normalizeAccountEmail(email)
	now := s.now()
	if allowed, ok := s.access.get(email, now); ok {
		return allowed
	}
	ctx, cancel := context.WithTimeout(context.Background(), accessLookupLimit)
	defer cancel()
	var disabled bool
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM users WHERE email = $1 AND status = 'disabled')`,
		email).Scan(&disabled)
	if err != nil {
		log.Printf("[Account] access check failed, allowing: %v", err)
		s.access.put(email, true, now.Add(accessFailOpenTTL))
		return true
	}
	s.access.put(email, !disabled, now.Add(accessCacheTTL))
	return !disabled
}
