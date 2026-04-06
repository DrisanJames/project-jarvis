package pmta

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClassifyBounce(t *testing.T) {
	tests := []struct {
		cat  string
		want bounceClass
		desc string
	}{
		{"hard", bounceHard, "generic hard"},
		{"bad-mailbox", bounceHard, "invalid mailbox"},
		{"bad-domain", bounceHard, "invalid domain"},
		{"inactive-mailbox", bounceHard, "inactive mailbox"},
		{"no-answer-from-host", bounceHard, "connection refused"},

		{"spam-related", bounceReputation, "spam block"},
		{"policy-related", bounceReputation, "policy rejection"},
		{"routing-errors", bounceReputation, "routing/policy error"},

		{"quota-issues", bounceSoft, "quota full"},
		{"rate-limit", bounceSoft, "rate limited"},
		{"bad-connection", bounceSoft, "transient connection failure"},
		{"message-expired", bounceSoft, "message expired"},
		{"content-related", bounceSoft, "content filtering"},
		{"other", bounceSoft, "other"},
		{"", bounceSoft, "empty category"},
	}

	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			got := classifyBounce(tt.cat)
			assert.Equal(t, tt.want, got, "classifyBounce(%q)", tt.cat)
		})
	}
}

func TestIsHardBounceCat_BackwardCompat(t *testing.T) {
	assert.True(t, isHardBounceCat("bad-mailbox"))
	assert.True(t, isHardBounceCat("bad-domain"))
	assert.True(t, isHardBounceCat("inactive-mailbox"))

	assert.False(t, isHardBounceCat("spam-related"), "reputation block, not hard")
	assert.False(t, isHardBounceCat("policy-related"), "reputation block, not hard")
	assert.False(t, isHardBounceCat("bad-connection"), "transient, not hard")
	assert.False(t, isHardBounceCat("quota-issues"), "soft bounce")
}
