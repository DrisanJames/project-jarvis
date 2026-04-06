package pmta

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsHardBounceCat(t *testing.T) {
	hard := []string{"hard", "bad-mailbox", "bad-domain", "inactive-mailbox",
		"no-answer-from-host", "routing-errors", "policy-related", "bad-connection"}
	soft := []string{"quota-issues", "message-expired", "content-related", "other", ""}

	for _, c := range hard {
		assert.True(t, isHardBounceCat(c), "expected hard for %q", c)
	}
	for _, c := range soft {
		assert.False(t, isHardBounceCat(c), "expected soft for %q", c)
	}
}
