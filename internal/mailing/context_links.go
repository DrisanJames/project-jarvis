package mailing

import (
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ignite/sparkpost-monitor/internal/preferences"
)

// recipientLink mints the signed v1 preference link used for every
// unsubscribe / preferences URL this builder emits:
//
//	{baseURL}/track/preferences?t=<token>[&intent=unsubscribe]
//
// It replaces generateToken's md5(sid|cid|key)[:16], which no handler ever
// verified (GET /unsubscribe discarded it and globally unsubscribed whatever
// sid it was handed). GET never mutates: the link opens the preference page,
// with the unsubscribe choice up front when unsubscribeIntent is set. A brand
// scope with no resolvable brand root falls back to all-brands. With no
// signing key the link carries no token and lands on the neutral page.
func (cb *ContextBuilder) recipientLink(orgID, subscriberID uuid.UUID, brandRoot string, scope preferences.Scope, unsubscribeIntent bool) string {
	if scope == preferences.ScopeBrand && strings.TrimSpace(brandRoot) == "" {
		scope = preferences.ScopeAll
	}
	u := strings.TrimRight(cb.baseURL, "/") + "/track/preferences"
	tok := preferences.Mint(orgID.String(), subscriberID.String(), brandRoot, scope, time.Now(), cb.signingKey)
	if tok == "" {
		return u
	}
	u += "?t=" + tok
	if unsubscribeIntent {
		u += "&intent=unsubscribe"
	}
	return u
}
