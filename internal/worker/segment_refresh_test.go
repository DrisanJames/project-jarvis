package worker

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestExpandEmailContains_SingleDomain(t *testing.T) {
	clause, args, nextArg := expandEmailContains("email", "@cox.net", 1)
	assert.Equal(t, " AND s.email ILIKE $1", clause)
	assert.Equal(t, []interface{}{"%@cox.net%"}, args)
	assert.Equal(t, 2, nextArg)
}

func TestExpandEmailContains_MicrosoftOutlook(t *testing.T) {
	clause, args, nextArg := expandEmailContains("email", "@outlook.com", 1)
	assert.Contains(t, clause, "OR")
	assert.Equal(t, 8, len(args)) // unified Microsoft: outlook, outlook.co.uk, hotmail, hotmail.co.uk, hotmail.fr, live, live.co.uk, msn
	assert.Contains(t, args, "%@outlook.com%")
	assert.Contains(t, args, "%@hotmail.com%")
	assert.Contains(t, args, "%@live.com%")
	assert.Contains(t, args, "%@msn.com%")
	assert.Equal(t, 9, nextArg)
}

func TestExpandEmailContains_Charter(t *testing.T) {
	clause, args, nextArg := expandEmailContains("email", "@charter.net", 1)
	assert.Contains(t, clause, "OR")
	assert.Equal(t, 6, len(args)) // charter, spectrum, rr, roadrunner, twc, brighthouse
	assert.Contains(t, args, "%@charter.net%")
	assert.Contains(t, args, "%@spectrum.net%")
	assert.Contains(t, args, "%@rr.com%")
	assert.Equal(t, 7, nextArg)
}

func TestExpandEmailContains_Yahoo(t *testing.T) {
	_, args, nextArg := expandEmailContains("email", "@yahoo.com", 5)
	assert.Equal(t, 8, len(args)) // yahoo, ymail, rocketmail, yahoo.ca, yahoo.co.uk, yahoo.co.in, yahoo.com.au, yahoo.co.jp
	assert.Contains(t, args, "%@yahoo.com%")
	assert.Contains(t, args, "%@ymail.com%")
	assert.Contains(t, args, "%@rocketmail.com%")
	assert.Contains(t, args, "%@yahoo.co.uk%")
	assert.Equal(t, 13, nextArg)
}

func TestExpandEmailContains_Apple(t *testing.T) {
	_, args, _ := expandEmailContains("email", "@icloud.com", 1)
	assert.Equal(t, 3, len(args))
	assert.Contains(t, args, "%@icloud.com%")
	assert.Contains(t, args, "%@me.com%")
	assert.Contains(t, args, "%@mac.com%")
}

func TestExpandEmailContains_ATT_NoExpansion(t *testing.T) {
	// ATT sub-brands (bellsouth, pacbell, etc.) are separate ISPs in
	// daily_acquisition.py — att.net must NOT expand to include them.
	clause, args, nextArg := expandEmailContains("email", "@att.net", 1)
	assert.Equal(t, " AND s.email ILIKE $1", clause)
	assert.Equal(t, []interface{}{"%@att.net%"}, args)
	assert.Equal(t, 2, nextArg)
}

func TestExpandEmailContains_NonISPDomain(t *testing.T) {
	clause, args, nextArg := expandEmailContains("email", "@example.com", 1)
	assert.Equal(t, " AND s.email ILIKE $1", clause)
	assert.Equal(t, []interface{}{"%@example.com%"}, args)
	assert.Equal(t, 2, nextArg)
}

func TestExpandEmailContainsNonPrefixed_Charter(t *testing.T) {
	clause, args, nextArg := expandEmailContainsNonPrefixed("email", "@charter.net", 1)
	assert.Contains(t, clause, "OR")
	assert.NotContains(t, clause, "s.email")
	assert.Equal(t, 6, len(args)) // charter, spectrum, rr, roadrunner, twc, brighthouse
	assert.Contains(t, args, "%@charter.net%")
	assert.Contains(t, args, "%@spectrum.net%")
	assert.Equal(t, 7, nextArg)
}

func TestExpandEmailContainsNonPrefixed_CaseInsensitive(t *testing.T) {
	clause, args, _ := expandEmailContainsNonPrefixed("email", "@Outlook.com", 1)
	assert.Contains(t, clause, "OR")
	assert.Equal(t, 8, len(args)) // unified Microsoft
}
